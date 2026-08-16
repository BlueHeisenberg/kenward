package transport

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testToken = "123456:AAH-not-a-real-token"

func newTestTelegram(t *testing.T, api *fakeAPI, opts ...Option) *Telegram {
	t.Helper()
	base := []Option{
		WithAPIServer(api.URL()),
		WithPollTimeout(2 * time.Second),
	}
	tg, err := NewTelegram(testToken, append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	t.Cleanup(func() { _ = tg.Close() })
	return tg
}

func TestNewTelegramRejectsEmptyToken(t *testing.T) {
	if _, err := NewTelegram("  "); err == nil {
		t.Fatal("expected an error for an empty token")
	}
}

// A member's stream carries text from private chats and groups, and nothing else.
func TestUpdatesDeliversOnlyConversation(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates, err := tg.Updates(ctx)
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}

	api.push(photoUpdate(100, 7))
	api.push(textUpdate(100, 7, "private", "kettle needs descaling"))
	api.push(channelPostUpdate(-500, "broadcast"))
	api.push(botTextUpdate(100, 9, "i am a bot"))
	api.push(textUpdate(-200, 8, "supergroup", "who is cooking"))
	api.push(callbackUpdate("cb", 7, 100, 1, "unknown:0"))
	api.push(textUpdate(100, 7, "private", "last"))

	want := []struct {
		chat  int64
		user  int64
		text  string
		group bool
	}{
		{100, 7, "kettle needs descaling", false},
		{-200, 8, "who is cooking", true},
		{100, 7, "last", false},
	}

	for i, w := range want {
		select {
		case in := <-updates:
			if in.ChatID != w.chat || in.UserID != w.user || in.Text != w.text || in.IsGroup != w.group {
				t.Fatalf("update %d = %+v, want chat %d user %d text %q group %v",
					i, in, w.chat, w.user, w.text, w.group)
			}
			if in.At.IsZero() {
				t.Fatalf("update %d has no timestamp", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for update %d", i)
		}
	}

	select {
	case in, ok := <-updates:
		if ok {
			t.Fatalf("unexpected extra update %+v", in)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUpdatesIsSingleReader(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("first Updates: %v", err)
	}
	if _, err := tg.Updates(ctx); !errors.Is(err, ErrUpdatesActive) {
		t.Fatalf("second Updates error = %v, want ErrUpdatesActive", err)
	}
}

func TestUpdatesChannelClosesOnContextCancel(t *testing.T) {
	api := newFakeAPI(t)
	client := &http.Client{Timeout: 12 * time.Second}
	tg := newTestTelegram(t, api, WithHTTPClient(client))

	// Counted after the server and the token check, so what is measured is the
	// transport's own goroutines and nothing else.
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	updates, err := tg.Updates(ctx)
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}
	cancel()

	select {
	case _, ok := <-updates:
		if ok {
			// A message may already have been in flight; drain to closure.
			for range updates {
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("updates channel was not closed after cancellation")
	}

	if err := tg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	client.CloseIdleConnections()
	assertNoGoroutineLeak(t, base)
}

func TestSendSplitsOnParagraphs(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api, WithMaxMessageLength(40))

	text := strings.Join([]string{
		"First paragraph, short enough.",
		"Second paragraph, also fine.",
		"Third one, still under the cap.",
	}, "\n\n")

	if err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: text}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := api.callsFor("sendMessage")
	if len(calls) != 3 {
		t.Fatalf("sendMessage called %d times, want 3", len(calls))
	}
	var parts []string
	for i, c := range calls {
		got := c.Form.Get("text")
		if utf16Len(got) > 40 {
			t.Fatalf("part %d is %d units long, over the limit", i, utf16Len(got))
		}
		parts = append(parts, got)
	}
	if joined := strings.Join(parts, "\n\n"); joined != text {
		t.Fatalf("reassembled text does not match:\n got %q\nwant %q", joined, text)
	}
}

func TestSendNeverTruncatesAnUnbreakableRun(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api, WithMaxMessageLength(10))

	text := strings.Repeat("x", 35)
	if err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: text}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var got string
	for _, c := range api.callsFor("sendMessage") {
		part := c.Form.Get("text")
		if utf16Len(part) > 10 {
			t.Fatalf("part over the limit: %q", part)
		}
		got += part
	}
	if got != text {
		t.Fatalf("content lost in splitting: got %d chars, want %d", len(got), len(text))
	}
}

func TestSendRepliesOnlyOnTheFirstPart(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api, WithMaxMessageLength(20))

	err := tg.Send(context.Background(), Outbound{
		ChatID:  100,
		Text:    "one two three four five six seven eight",
		ReplyTo: 77,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := api.callsFor("sendMessage")
	if len(calls) < 2 {
		t.Fatalf("expected the message to be split, got %d parts", len(calls))
	}
	if got := calls[0].Form.Get("reply_parameters"); !strings.Contains(got, "77") {
		t.Fatalf("first part reply_parameters = %q, want the reply id", got)
	}
	for i, c := range calls[1:] {
		if got := c.Form.Get("reply_parameters"); got != "" {
			t.Fatalf("part %d also replied: %q", i+2, got)
		}
	}
}

func TestSendRejectsEmptyText(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	if err := tg.Send(context.Background(), Outbound{ChatID: 1, Text: "   "}); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("Send error = %v, want ErrEmptyText", err)
	}
	if n := api.countFor("sendMessage"); n != 0 {
		t.Fatalf("empty text still hit the API %d times", n)
	}
}

func TestSendHonoursRetryAfter(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	var sawRetryAfter int
	tg.retryDelay = func(retryAfter int) time.Duration {
		sawRetryAfter = retryAfter
		return time.Millisecond
	}

	api.script("sendMessage",
		`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`)

	if err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sawRetryAfter != 7 {
		t.Fatalf("retry_after honoured = %d, want 7", sawRetryAfter)
	}
	if n := api.countFor("sendMessage"); n != 2 {
		t.Fatalf("sendMessage attempted %d times, want 2", n)
	}
}

func TestSendGivesUpAfterRetryBudget(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api, WithRateLimitRetries(1))
	tg.retryDelay = func(int) time.Duration { return time.Millisecond }

	for i := 0; i < 4; i++ {
		api.script("sendMessage",
			`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
	}

	err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: "hello"})
	if err == nil {
		t.Fatal("expected the rate limit to surface once the budget ran out")
	}
	if n := api.countFor("sendMessage"); n != 2 {
		t.Fatalf("sendMessage attempted %d times, want 2 (one try, one retry)", n)
	}
}

// askInFlight starts an Ask and returns the question's callback token prefix and
// a channel carrying the result.
func askInFlight(t *testing.T, api *fakeAPI, tg *Telegram, q Question) (data []string, result chan askResult) {
	t.Helper()
	result = make(chan askResult, 1)
	go func() {
		a, err := tg.Ask(context.Background(), q)
		result <- askResult{a, err}
	}()

	sent := api.waitCall(t, "sendMessage", 1)
	if got := sent.Form.Get("text"); got != q.Text {
		t.Fatalf("question text = %q, want %q", got, q.Text)
	}
	rows := keyboardFromForm(t, sent)
	if len(rows) != len(q.Choices) {
		t.Fatalf("keyboard has %d rows, want %d", len(rows), len(q.Choices))
	}
	for _, row := range rows {
		data = append(data, row[0].CallbackData)
	}
	return data, result
}

type askResult struct {
	answer Answer
	err    error
}

func TestAskReturnsTheChoiceTheMemberTapped(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}

	q := Question{
		ChatID:        100,
		Text:          "Where should this go?",
		Choices:       []Choice{{ID: "personal", Label: "Personal"}, {ID: "shared", Label: "Household"}},
		AllowedUserID: 7,
		Timeout:       3 * time.Second,
	}
	data, result := askInFlight(t, api, tg, q)

	api.push(callbackUpdate("cb1", 7, 100, 1001, data[1]))

	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if r.answer.ChoiceID != "shared" || r.answer.UserID != 7 || r.answer.TimedOut {
			t.Fatalf("answer = %+v, want shared from 7", r.answer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return after the member tapped")
	}

	if n := api.countFor("answerCallbackQuery"); n != 1 {
		t.Fatalf("answerCallbackQuery called %d times, want 1", n)
	}
	edit := api.waitCall(t, "editMessageText", 1)
	if !strings.Contains(edit.Form.Get("text"), "Household") {
		t.Fatalf("edited message %q does not show the outcome", edit.Form.Get("text"))
	}
	if got := edit.Form.Get("reply_markup"); !strings.Contains(got, `"inline_keyboard":[]`) {
		t.Fatalf("keyboard not removed: reply_markup = %q", got)
	}
}

// The security case: in a group chat everyone can see the keyboard. A tap from
// anyone but the addressed member must change nothing at all.
func TestAskIgnoresTapsFromOtherMembers(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}

	q := Question{
		ChatID:        -200,
		Text:          "Save this to the household memory?",
		Choices:       []Choice{{ID: "shared", Label: "Household"}, {ID: "no", Label: "Don't save"}},
		AllowedUserID: 7,
		Timeout:       3 * time.Second,
	}
	data, result := askInFlight(t, api, tg, q)

	// Somebody else in the group taps both buttons.
	api.push(callbackUpdate("cb-intruder-1", 8, -200, 1001, data[0]))
	api.push(callbackUpdate("cb-intruder-2", 999, -200, 1001, data[1]))

	select {
	case r := <-result:
		t.Fatalf("Ask returned %+v for a tap from the wrong member", r)
	case <-time.After(300 * time.Millisecond):
	}

	if n := api.countFor("answerCallbackQuery"); n != 0 {
		t.Fatalf("an intruder's tap was acknowledged %d times, want 0", n)
	}
	if n := api.countFor("editMessageText"); n != 0 {
		t.Fatalf("an intruder's tap edited the question %d times, want 0", n)
	}

	// The addressed member's tap still works.
	api.push(callbackUpdate("cb-owner", 7, -200, 1001, data[0]))
	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if r.answer.ChoiceID != "shared" || r.answer.UserID != 7 {
			t.Fatalf("answer = %+v, want shared from 7", r.answer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return after the addressed member tapped")
	}
}

// A question only the wrong people touch times out, and a timeout is a decline.
func TestAskTimesOutWhenOnlyTheWrongMemberTaps(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}

	q := Question{
		ChatID:        -200,
		Text:          "Save this?",
		Choices:       []Choice{{ID: "yes", Label: "Save"}, {ID: "no", Label: "Don't save"}},
		AllowedUserID: 7,
		Timeout:       400 * time.Millisecond,
	}
	data, result := askInFlight(t, api, tg, q)
	api.push(callbackUpdate("cb-intruder", 8, -200, 1001, data[0]))

	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if !r.answer.TimedOut {
			t.Fatalf("answer = %+v, want a timeout", r.answer)
		}
		if r.answer.ChoiceID != "" {
			t.Fatalf("a timed-out answer carried a choice: %+v", r.answer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask never timed out")
	}

	if n := api.countFor("answerCallbackQuery"); n != 0 {
		t.Fatalf("the intruder's tap was acknowledged %d times, want 0", n)
	}
	edit := api.waitCall(t, "editMessageText", 1)
	if !strings.Contains(edit.Form.Get("text"), "declined") {
		t.Fatalf("timed-out question reads %q, want it to say it was declined", edit.Form.Get("text"))
	}
}

func TestAskTimeoutRemovesTheKeyboard(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}

	q := Question{
		ChatID:        100,
		Text:          "Save this?",
		Choices:       []Choice{{ID: "yes", Label: "Save"}},
		AllowedUserID: 7,
		Timeout:       150 * time.Millisecond,
	}
	answer, err := tg.Ask(context.Background(), q)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !answer.TimedOut {
		t.Fatalf("answer = %+v, want a timeout", answer)
	}
	edit := api.waitCall(t, "editMessageText", 1)
	if got := edit.Form.Get("reply_markup"); !strings.Contains(got, `"inline_keyboard":[]`) {
		t.Fatalf("keyboard not removed on timeout: %q", got)
	}
}

// A keyboard left on screen must not be tappable twice.
func TestAskIgnoresASecondTap(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}

	q := Question{
		ChatID:        100,
		Text:          "Save this?",
		Choices:       []Choice{{ID: "yes", Label: "Save"}, {ID: "no", Label: "Don't save"}},
		AllowedUserID: 7,
		Timeout:       3 * time.Second,
	}
	data, result := askInFlight(t, api, tg, q)

	api.push(callbackUpdate("cb1", 7, 100, 1001, data[0]))
	select {
	case r := <-result:
		if r.err != nil || r.answer.ChoiceID != "yes" {
			t.Fatalf("Ask = %+v, %v", r.answer, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return")
	}

	api.waitCall(t, "editMessageText", 1)
	api.push(callbackUpdate("cb2", 7, 100, 1001, data[1]))
	time.Sleep(200 * time.Millisecond)

	if n := api.countFor("answerCallbackQuery"); n != 1 {
		t.Fatalf("answerCallbackQuery called %d times, want 1", n)
	}
	if n := api.countFor("editMessageText"); n != 1 {
		t.Fatalf("editMessageText called %d times, want 1", n)
	}
}

func TestAskValidates(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api, WithMaxMessageLength(100))

	cases := []struct {
		name string
		q    Question
		want error
	}{
		{"no chat", Question{Text: "x", Choices: []Choice{{ID: "a", Label: "A"}}}, nil},
		{"no text", Question{ChatID: 1, Choices: []Choice{{ID: "a", Label: "A"}}}, ErrEmptyText},
		{"no choices", Question{ChatID: 1, Text: "x"}, ErrNoChoices},
		{"too long", Question{ChatID: 1, Text: strings.Repeat("y", 90), Choices: []Choice{{ID: "a", Label: "A"}}}, ErrTextTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tg.Ask(context.Background(), tc.q)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	if n := api.countFor("sendMessage"); n != 0 {
		t.Fatalf("an invalid question still reached Telegram %d times", n)
	}
}

func TestAskUnblocksOnClose(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	result := make(chan askResult, 1)
	go func() {
		a, err := tg.Ask(context.Background(), Question{
			ChatID:        100,
			Text:          "Save this?",
			Choices:       []Choice{{ID: "yes", Label: "Save"}},
			AllowedUserID: 7,
			Timeout:       10 * time.Second,
		})
		result <- askResult{a, err}
	}()

	api.waitCall(t, "sendMessage", 1)
	if err := tg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case r := <-result:
		if !errors.Is(r.err, ErrClosed) {
			t.Fatalf("Ask error = %v, want ErrClosed", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not unblock on Close")
	}
}

func TestAskRespectsContextCancellation(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan askResult, 1)
	go func() {
		a, err := tg.Ask(ctx, Question{
			ChatID:        100,
			Text:          "Save this?",
			Choices:       []Choice{{ID: "yes", Label: "Save"}},
			AllowedUserID: 7,
			Timeout:       10 * time.Second,
		})
		result <- askResult{a, err}
	}()

	api.waitCall(t, "sendMessage", 1)
	cancel()

	select {
	case r := <-result:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Ask error = %v, want context.Canceled", r.err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Ask did not unblock on cancellation")
	}
	// The keyboard is still withdrawn, on a context of its own.
	api.waitCall(t, "editMessageText", 1)
}

func TestCloseIsIdempotentAndFinal(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	if err := tg.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tg.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := tg.Send(context.Background(), Outbound{ChatID: 1, Text: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close = %v, want ErrClosed", err)
	}
	if _, err := tg.Ask(context.Background(), Question{ChatID: 1, Text: "x", Choices: []Choice{{ID: "a", Label: "A"}}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ask after Close = %v, want ErrClosed", err)
	}
	if _, err := tg.Updates(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Updates after Close = %v, want ErrClosed", err)
	}
}

// The Updates/Close WaitGroup race: Close must never return while pumps it did
// not wait for are about to start. The gate runs at the exact point the old code
// had released the lock, so this interleaving is forced, not hoped for.
func TestUpdatesRegistersPumpsBeforeCloseCanWait(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	gateRan := false
	tg.updatesGate = func() {
		gateRan = true
		if err := tg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Close returned; if it waited for the pumps as it must, the poll pump
		// has already run to completion and closed the queue. Under the old
		// ordering the pumps were not yet registered here, Close saw a zero
		// counter, and the queue was still open.
		tg.queue.mu.Lock()
		closed := tg.queue.closed
		tg.queue.mu.Unlock()
		if !closed {
			t.Fatal("Close returned while the pumps had not been waited for")
		}
	}

	updates, err := tg.Updates(context.Background())
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}
	if !gateRan {
		t.Fatal("the gate never ran")
	}
	select {
	case _, ok := <-updates:
		if ok {
			for range updates {
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("updates channel was not closed after Close")
	}
}

// A callback with no sender must never match a question — not even one whose
// AllowedUserID was left zero by a buggy caller — and must never crash the poll
// goroutine, which runs handlers synchronously.
func TestCallbackWithoutSenderIsIgnored(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}

	q := Question{
		ChatID:        100,
		Text:          "Save this?",
		Choices:       []Choice{{ID: "yes", Label: "Save"}},
		AllowedUserID: 0, // unset by a buggy caller: still must not match a zero sender
		Timeout:       400 * time.Millisecond,
	}
	data, result := askInFlight(t, api, tg, q)

	api.push(callbackUpdateNoFrom("cb-nofrom", 100, 1001, data[0]))

	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if !r.answer.TimedOut {
			t.Fatalf("a sender-less callback was accepted: %+v", r.answer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask never returned")
	}
	if n := api.countFor("answerCallbackQuery"); n != 0 {
		t.Fatalf("a sender-less callback was acknowledged %d times, want 0", n)
	}
}

// The length check must reserve room for the outcome line actually appended on
// retirement, including the longest choice label.
func TestAskReservesRoomForTheOutcomeLine(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api, WithMaxMessageLength(100))

	// 30 units of text passes a flat reserve, but the 80-unit label pushes the
	// retired text to 114 units: it must be refused up front.
	long := Question{
		ChatID:        100,
		Text:          strings.Repeat("y", 30),
		Choices:       []Choice{{ID: "a", Label: strings.Repeat("L", 80)}},
		AllowedUserID: 7,
		Timeout:       150 * time.Millisecond,
	}
	if _, err := tg.Ask(context.Background(), long); !errors.Is(err, ErrTextTooLong) {
		t.Fatalf("Ask with an oversized label = %v, want ErrTextTooLong", err)
	}
	if n := api.countFor("sendMessage"); n != 0 {
		t.Fatalf("an oversized question still reached Telegram %d times", n)
	}

	// 50 units of text with a short label retires at 84 units and must be
	// allowed; a flat 64-unit reserve wrongly refused it.
	fits := Question{
		ChatID:        100,
		Text:          strings.Repeat("y", 50),
		Choices:       []Choice{{ID: "a", Label: "A"}},
		AllowedUserID: 7,
		Timeout:       150 * time.Millisecond,
	}
	answer, err := tg.Ask(context.Background(), fits)
	if err != nil {
		t.Fatalf("Ask that fits with its outcome line = %v, want nil", err)
	}
	if !answer.TimedOut {
		t.Fatalf("answer = %+v, want a timeout", answer)
	}
}

// After Close, an in-flight Ask's cleanup edit must be bounded rather than
// hanging on the caller's live context for the full HTTP client timeout.
func TestCloseBoundsAskCleanup(t *testing.T) {
	api := newFakeAPI(t)
	tg := newTestTelegram(t, api)
	release := api.holdMethod("editMessageText")
	defer release()

	result := make(chan askResult, 1)
	go func() {
		a, err := tg.Ask(context.Background(), Question{
			ChatID:        100,
			Text:          "Save this?",
			Choices:       []Choice{{ID: "yes", Label: "Save"}},
			AllowedUserID: 7,
			Timeout:       30 * time.Second,
		})
		result <- askResult{a, err}
	}()

	api.waitCall(t, "sendMessage", 1)
	start := time.Now()
	if err := tg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case r := <-result:
		if !errors.Is(r.err, ErrClosed) {
			t.Fatalf("Ask error = %v, want ErrClosed", r.err)
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Fatalf("Ask blocked %v after Close; the cleanup edit must be bounded", elapsed)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Ask still blocked on its cleanup edit long after Close returned")
	}
}

func TestRedactTokenScrubsEncodedForms(t *testing.T) {
	// A URL-encoded token (":" becomes "%3A") must not slip through the scrub.
	inner := errors.New("proxy error for /bot" + strings.ReplaceAll(testToken, ":", "%3A") + "/sendMessage")
	err := redactToken(inner, testToken)
	if strings.Contains(err.Error(), "AAH-not-a-real-token") {
		t.Fatalf("encoded token survived redaction: %q", err.Error())
	}
	if !errors.Is(err, inner) {
		t.Fatal("redaction broke the error chain")
	}
}

func TestRedactTokenKeepsTheChain(t *testing.T) {
	inner := errors.New("bad request from https://api.telegram.org/bot" + testToken + "/sendMessage")
	err := redactToken(inner, testToken)
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token survived redaction: %q", err.Error())
	}
	if !errors.Is(err, inner) {
		t.Fatal("redaction broke the error chain")
	}
}

func TestParseCallbackData(t *testing.T) {
	cases := []struct {
		in    string
		token string
		idx   int
		ok    bool
	}{
		{"abc123:0", "abc123", 0, true},
		{"abc123:12", "abc123", 12, true},
		{"abc123", "", 0, false},
		{":0", "", 0, false},
		{"abc123:", "", 0, false},
		{"abc123:x", "", 0, false},
		{"", "", 0, false},
	}
	for _, tc := range cases {
		token, idx, ok := parseCallbackData(tc.in)
		if ok != tc.ok || token != tc.token || idx != tc.idx {
			t.Fatalf("parseCallbackData(%q) = %q, %d, %v; want %q, %d, %v",
				tc.in, token, idx, ok, tc.token, tc.idx, tc.ok)
		}
	}
}
