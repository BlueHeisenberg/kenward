package transport

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/transport/telegramtest"
)

const testToken = "123456:AAH-not-a-real-token"

func newTestTelegram(t *testing.T, api *telegramtest.Server, opts ...Option) *Telegram {
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
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates, err := tg.Updates(ctx)
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}

	api.Push(telegramtest.PhotoUpdate(100, 7))
	api.Push(telegramtest.TextUpdate(100, 7, "private", "kettle needs descaling"))
	api.Push(telegramtest.ChannelPostUpdate(-500, "broadcast"))
	api.Push(telegramtest.BotTextUpdate(100, 9, "i am a bot"))
	api.Push(telegramtest.TextUpdate(-200, 8, "supergroup", "who is cooking"))
	api.Push(telegramtest.CallbackUpdate("cb", 7, 100, 1, "unknown:0"))
	api.Push(telegramtest.TextUpdate(100, 7, "private", "last"))

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
	api := telegramtest.New(t, testToken)
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
	api := telegramtest.New(t, testToken)
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
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api, WithMaxMessageLength(40))

	text := strings.Join([]string{
		"First paragraph, short enough.",
		"Second paragraph, also fine.",
		"Third one, still under the cap.",
	}, "\n\n")

	if err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: text}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := api.CallsFor("sendMessage")
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
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api, WithMaxMessageLength(10))

	text := strings.Repeat("x", 35)
	if err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: text}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var got string
	for _, c := range api.CallsFor("sendMessage") {
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
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api, WithMaxMessageLength(20))

	err := tg.Send(context.Background(), Outbound{
		ChatID:  100,
		Text:    "one two three four five six seven eight",
		ReplyTo: 77,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	calls := api.CallsFor("sendMessage")
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
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	if err := tg.Send(context.Background(), Outbound{ChatID: 1, Text: "   "}); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("Send error = %v, want ErrEmptyText", err)
	}
	if n := api.CountFor("sendMessage"); n != 0 {
		t.Fatalf("empty text still hit the API %d times", n)
	}
}

func TestSendHonoursRetryAfter(t *testing.T) {
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	var sawRetryAfter int
	tg.retryDelay = func(retryAfter int) time.Duration {
		sawRetryAfter = retryAfter
		return time.Millisecond
	}

	api.Script("sendMessage",
		`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`)

	if err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sawRetryAfter != 7 {
		t.Fatalf("retry_after honoured = %d, want 7", sawRetryAfter)
	}
	if n := api.CountFor("sendMessage"); n != 2 {
		t.Fatalf("sendMessage attempted %d times, want 2", n)
	}
}

func TestSendGivesUpAfterRetryBudget(t *testing.T) {
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api, WithRateLimitRetries(1))
	tg.retryDelay = func(int) time.Duration { return time.Millisecond }

	for i := 0; i < 4; i++ {
		api.Script("sendMessage",
			`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
	}

	err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: "hello"})
	if err == nil {
		t.Fatal("expected the rate limit to surface once the budget ran out")
	}
	if n := api.CountFor("sendMessage"); n != 2 {
		t.Fatalf("sendMessage attempted %d times, want 2 (one try, one retry)", n)
	}
}

// askInFlight starts an Ask and returns the question's callback token prefix and
// a channel carrying the result.
func askInFlight(t *testing.T, api *telegramtest.Server, tg *Telegram, q Question) (data []string, result chan askResult) {
	t.Helper()
	result = make(chan askResult, 1)
	go func() {
		a, err := tg.Ask(context.Background(), q)
		result <- askResult{a, err}
	}()

	sent := api.WaitCall(t, "sendMessage", 1)
	if got := sent.Form.Get("text"); got != q.Text {
		t.Fatalf("question text = %q, want %q", got, q.Text)
	}
	rows := telegramtest.Keyboard(t, sent)
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
	api := telegramtest.New(t, testToken)
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

	api.Push(telegramtest.CallbackUpdate("cb1", 7, 100, 1001, data[1]))

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

	if n := api.CountFor("answerCallbackQuery"); n != 1 {
		t.Fatalf("answerCallbackQuery called %d times, want 1", n)
	}
	edit := api.WaitCall(t, "editMessageText", 1)
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
	api := telegramtest.New(t, testToken)
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
	api.Push(telegramtest.CallbackUpdate("cb-intruder-1", 8, -200, 1001, data[0]))
	api.Push(telegramtest.CallbackUpdate("cb-intruder-2", 999, -200, 1001, data[1]))

	select {
	case r := <-result:
		t.Fatalf("Ask returned %+v for a tap from the wrong member", r)
	case <-time.After(300 * time.Millisecond):
	}

	if n := api.CountFor("answerCallbackQuery"); n != 0 {
		t.Fatalf("an intruder's tap was acknowledged %d times, want 0", n)
	}
	if n := api.CountFor("editMessageText"); n != 0 {
		t.Fatalf("an intruder's tap edited the question %d times, want 0", n)
	}

	// The addressed member's tap still works.
	api.Push(telegramtest.CallbackUpdate("cb-owner", 7, -200, 1001, data[0]))
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
	api := telegramtest.New(t, testToken)
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
	api.Push(telegramtest.CallbackUpdate("cb-intruder", 8, -200, 1001, data[0]))

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

	if n := api.CountFor("answerCallbackQuery"); n != 0 {
		t.Fatalf("the intruder's tap was acknowledged %d times, want 0", n)
	}
	edit := api.WaitCall(t, "editMessageText", 1)
	if !strings.Contains(edit.Form.Get("text"), "declined") {
		t.Fatalf("timed-out question reads %q, want it to say it was declined", edit.Form.Get("text"))
	}
}

func TestAskTimeoutRemovesTheKeyboard(t *testing.T) {
	api := telegramtest.New(t, testToken)
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
	edit := api.WaitCall(t, "editMessageText", 1)
	if got := edit.Form.Get("reply_markup"); !strings.Contains(got, `"inline_keyboard":[]`) {
		t.Fatalf("keyboard not removed on timeout: %q", got)
	}
}

// A keyboard left on screen must not be tappable twice.
func TestAskIgnoresASecondTap(t *testing.T) {
	api := telegramtest.New(t, testToken)
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

	api.Push(telegramtest.CallbackUpdate("cb1", 7, 100, 1001, data[0]))
	select {
	case r := <-result:
		if r.err != nil || r.answer.ChoiceID != "yes" {
			t.Fatalf("Ask = %+v, %v", r.answer, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return")
	}

	api.WaitCall(t, "editMessageText", 1)
	api.Push(telegramtest.CallbackUpdate("cb2", 7, 100, 1001, data[1]))
	time.Sleep(200 * time.Millisecond)

	if n := api.CountFor("answerCallbackQuery"); n != 1 {
		t.Fatalf("answerCallbackQuery called %d times, want 1", n)
	}
	if n := api.CountFor("editMessageText"); n != 1 {
		t.Fatalf("editMessageText called %d times, want 1", n)
	}
}

func TestAskValidates(t *testing.T) {
	api := telegramtest.New(t, testToken)
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
	if n := api.CountFor("sendMessage"); n != 0 {
		t.Fatalf("an invalid question still reached Telegram %d times", n)
	}
}

func TestAskUnblocksOnClose(t *testing.T) {
	api := telegramtest.New(t, testToken)
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

	api.WaitCall(t, "sendMessage", 1)
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
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan askResult, 1)
	// Posted is the barrier, not the sendMessage call. The subject here is what
	// cancelling does to a question that is already on screen, and waiting on the
	// call would only prove the server had answered — leaving the cancellation to
	// race Ask's own read of that answer. A send that loses the race fails with
	// context.Canceled, which satisfies the error assertion below while leaving no
	// message to retire, so the race would surface as the editMessageText that
	// never comes. Posted fires once the id is in hand and the question is
	// registered, which is exactly the state the test means to interrupt.
	posted := make(chan struct{})
	go func() {
		a, err := tg.Ask(ctx, Question{
			ChatID:        100,
			Text:          "Save this?",
			Choices:       []Choice{{ID: "yes", Label: "Save"}},
			AllowedUserID: 7,
			Timeout:       10 * time.Second,
			Posted:        func(int) { close(posted) },
		})
		result <- askResult{a, err}
	}()

	select {
	case <-posted:
	case <-time.After(4 * time.Second):
		t.Fatal("the question was never posted")
	}
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
	api.WaitCall(t, "editMessageText", 1)
}

func TestCloseIsIdempotentAndFinal(t *testing.T) {
	api := telegramtest.New(t, testToken)
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

// pumpRegistrationWindow is how long the gate below watches for a WaitGroup that
// has already fallen to zero. It is one-sided: a correct transport can never
// close the channel during it, because nothing has been cancelled yet and the
// poll pump will not exit, so the test cannot flake. A broken one closes it in
// microseconds — the goroutine only has to reach a Wait that returns at once.
const pumpRegistrationWindow = 200 * time.Millisecond

// The Updates/Close WaitGroup race: Close must never return while pumps it did
// not wait for are about to start. Updates therefore does wg.Add(2) under t.mu,
// which Close also takes, so Close either loses the started check at the top or
// finds a counter of 2.
//
// This test needs the updatesGate seam, and the seam had to move under the lock
// to earn its keep. The broken window — between the unlock and wg.Add(2) — is a
// few instructions on one goroutine with nothing blocking inside it, so a racing
// Close loses essentially every time: repetition, scheduling pressure and -race
// all pass on the bug, which is exactly how the previous version of this test
// came to guard nothing. The only place the two orderings differ observably is
// the instant t.mu is released, so that is where the gate looks, and what it
// asks is whether wg.Wait() would return there.
func TestUpdatesRegistersPumpsBeforeCloseCanWait(t *testing.T) {
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	gateRan := false
	tg.updatesGate = func() {
		// Called with t.mu held, immediately before Updates releases it. A Close
		// blocked on that lock resumes from here, so this is where the WaitGroup
		// must already be non-zero.
		gateRan = true

		waitReturned := make(chan struct{})
		go func() {
			tg.wg.Wait()
			close(waitReturned)
		}()

		select {
		case <-waitReturned:
			// Not t.Fatal: Goexit here would strand t.mu and deadlock the
			// cleanup Close.
			t.Error("Updates was about to release t.mu with a zero WaitGroup: " +
				"a Close taking the lock next would wait for nothing and return " +
				"while both pumps ran behind its back")
		case <-time.After(pumpRegistrationWindow):
			// Still counted, as it must be: nothing has been cancelled and the
			// poll pump is polling, so Wait cannot legitimately return here.
		}
	}

	updates, err := tg.Updates(context.Background())
	if err != nil {
		t.Fatalf("Updates: %v", err)
	}
	if !gateRan {
		t.Fatal("the gate never ran")
	}

	// And the property the registration exists for: Close waits for both pumps,
	// so the stream is closed and drained by the time it returns.
	if err := tg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-updates:
		if ok {
			for range updates {
			}
		}
	default:
		t.Fatal("Close returned with the updates channel still open")
	}
}

// A callback with no sender must never match a question — not even one whose
// AllowedUserID was left zero by a buggy caller — and must never crash the poll
// goroutine, which runs handlers synchronously.
func TestCallbackWithoutSenderIsIgnored(t *testing.T) {
	api := telegramtest.New(t, testToken)
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

	api.Push(telegramtest.CallbackUpdateNoFrom("cb-nofrom", 100, 1001, data[0]))

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
	if n := api.CountFor("answerCallbackQuery"); n != 0 {
		t.Fatalf("a sender-less callback was acknowledged %d times, want 0", n)
	}
}

// The length check must reserve room for the outcome line actually appended on
// retirement, including the longest choice label.
func TestAskReservesRoomForTheOutcomeLine(t *testing.T) {
	api := telegramtest.New(t, testToken)
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
	if n := api.CountFor("sendMessage"); n != 0 {
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
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)
	release := api.Hold("editMessageText")
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

	api.WaitCall(t, "sendMessage", 1)
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

// assertNoGoroutineLeak fails if the goroutine count has not returned to base.
// It retries, because a goroutine that is on its way out has not always left by
// the time the test asks.
func assertNoGoroutineLeak(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.Gosched()
		now := runtime.NumGoroutine()
		if now <= base {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak: %d at start, %d now\n%s", base, now, buf[:n])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRetiredNoteReplacesTheDeclineWording is for the shape of question that is not
// a question: a message reporting something already done, offering one button to
// take it back.
//
// The default retirement line says the question was declined. Appended to "I've
// written this to your private memory", that reads as the write having been called
// off — the one thing such a message must never say, because a member who believes
// it will not go looking for an entry that is really there.
func TestRetiredNoteReplacesTheDeclineWording(t *testing.T) {
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}

	q := Question{
		ChatID:        -200,
		Text:          "I've written this to your private memory.",
		Choices:       []Choice{{ID: "undo", Label: "Undo"}},
		AllowedUserID: 7,
		Timeout:       200 * time.Millisecond,
		RetiredNote:   "the undo window has closed; this is still in memory",
	}
	_, result := askInFlight(t, api, tg, q)

	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if !r.answer.TimedOut {
			t.Fatalf("answer = %+v, want a timeout", r.answer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask never timed out")
	}

	edit := api.WaitCall(t, "editMessageText", 1)
	got := edit.Form.Get("text")
	if !strings.Contains(got, q.Text) {
		t.Errorf("retired message %q dropped the original text; the established pattern keeps it and appends the outcome", got)
	}
	if !strings.Contains(got, q.RetiredNote) {
		t.Errorf("retired message %q does not carry the caller's note", got)
	}
	if strings.Contains(got, "declined") {
		t.Errorf("retired message %q says the announcement was declined; the write it reports still stands", got)
	}
}

// TestRetireReserveCountsTheRetiredNote. Ask refuses a text too long to survive its
// own retirement edit, and the reservation is computed from the outcome functions so
// it cannot drift from what retire actually writes. A note longer than every choice
// label and every default line has to be in that computation, or a message is
// accepted that cannot be edited afterwards — leaving a live keyboard on a decision
// that has already expired.
func TestRetireReserveCountsTheRetiredNote(t *testing.T) {
	note := strings.Repeat("x", 120)
	q := Question{
		Text:        "written",
		Choices:     []Choice{{ID: "undo", Label: "Undo"}},
		RetiredNote: note,
	}
	withNote := retireReserve(q)
	q.RetiredNote = ""
	without := retireReserve(q)
	if withNote <= without {
		t.Fatalf("retireReserve with a %d-character note = %d, without = %d; the note is not reserved for",
			len(note), withNote, without)
	}
	if withNote < utf16Len(note) {
		t.Errorf("reserve %d is smaller than the note itself (%d)", withNote, utf16Len(note))
	}
}

// --- parse mode and its failure mode ---------------------------------------

// Every message goes out with the parse mode set. Nothing did before this, which
// is why refusals shipped literal backticks and every announcement was a wall of
// flat text.
func TestSendSetsTheParseMode(t *testing.T) {
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	text := Bold("Bin day") + "\n" + Quote("Thursday.")
	if err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: text}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	calls := api.CallsFor("sendMessage")
	if len(calls) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(calls))
	}
	if got := calls[0].Form.Get("parse_mode"); got != "HTML" {
		t.Errorf("parse_mode = %q, want HTML", got)
	}
	if got := calls[0].Form.Get("text"); got != text {
		t.Errorf("text = %q, want the formatted text %q", got, text)
	}
}

// A question and the edit that retires it are formatted too — the outcome line is
// italic, and losing that edit would leave a live keyboard on a settled decision.
func TestAskAndRetireSetTheParseMode(t *testing.T) {
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api, WithQuestionTimeout(50*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tg.Updates(ctx); err != nil {
		t.Fatalf("Updates: %v", err)
	}

	ans, err := tg.Ask(ctx, Question{
		ChatID:  100,
		Text:    Bold("Bins go out Tuesday"),
		Choices: []Choice{{ID: "save", Label: "Save"}},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !ans.TimedOut {
		t.Fatalf("Ask = %+v, want a timeout", ans)
	}
	for _, method := range []string{"sendMessage", "editMessageText"} {
		calls := api.CallsFor(method)
		if len(calls) == 0 {
			t.Fatalf("no %s call", method)
		}
		if got := calls[0].Form.Get("parse_mode"); got != "HTML" {
			t.Errorf("%s parse_mode = %q, want HTML", method, got)
		}
	}
	if got := api.CallsFor("editMessageText")[0].Form.Get("text"); !strings.Contains(got, "<i>— no answer") {
		t.Errorf("retired text = %q, want the outcome line in italics", got)
	}
}

// Telegram rejecting a formatted message must cost the member their styling and
// never their message. A 400 that swallowed a memory confirmation would be far
// worse than an unstyled one.
func TestRejectedFormattingIsResentAsPlainText(t *testing.T) {
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	api.ScriptStatus("sendMessage", http.StatusBadRequest,
		`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: Unsupported start tag \"b\" at byte offset 0"}`)

	text := Bold("Bin day") + " — " + Quote("Thursday & Friday.")
	if err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: text}); err != nil {
		t.Fatalf("Send: %v, want the plain-text resend to succeed", err)
	}

	calls := api.CallsFor("sendMessage")
	if len(calls) != 2 {
		t.Fatalf("sendMessage calls = %d, want the rejected one and the resend", len(calls))
	}
	if got := calls[1].Form.Get("parse_mode"); got != "" {
		t.Errorf("resend parse_mode = %q, want it unset", got)
	}
	want := "Bin day — Thursday & Friday."
	if got := calls[1].Form.Get("text"); got != want {
		t.Errorf("resend text = %q, want the words with the markup stripped: %q", got, want)
	}
}

// A 400 that is not about formatting still gets one plain-text attempt, and when
// that fails too the caller hears about it rather than believing the send worked.
func TestASendThatFailsBothWaysIsStillAnError(t *testing.T) {
	api := telegramtest.New(t, testToken)
	tg := newTestTelegram(t, api)

	for i := 0; i < 2; i++ {
		api.ScriptStatus("sendMessage", http.StatusBadRequest,
			`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	}
	err := tg.Send(context.Background(), Outbound{ChatID: 100, Text: Bold("Bin day")})
	if err == nil {
		t.Fatal("Send returned nil; a message that never arrived must not look delivered")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error = %v, want it to name the real fault", err)
	}
}

// TestRetireReserveMeasuresTheQuestionsOwnLanguage. Every input to the reservation is
// language-dependent: the two outcome phrases, the dash that introduces them, the
// caller's retired note and every button label. A margin tuned on English is silently
// wrong in a language whose outcome line is longer, and the failure is a 400 from
// Telegram on a long question rather than anything a test would otherwise see.
func TestRetireReserveMeasuresTheQuestionsOwnLanguage(t *testing.T) {
	english := Question{Notes: OutcomeNotes{}}
	longer := Question{Notes: OutcomeNotes{
		Dash:      "— ",
		Declined:  "geen antwoord, geldt als geweigerd en dan nog wat woorden erbij",
		Withdrawn: "vraag ingetrokken",
	}}
	if retireReserve(longer) <= retireReserve(english) {
		t.Errorf("a longer outcome line reserved %d units against English's %d",
			retireReserve(longer), retireReserve(english))
	}
	// The reservation is what retire actually writes, so the two cannot drift.
	if got, want := retireReserve(longer), utf16Len(declinedText(longer)); got != want {
		t.Errorf("reserve = %d, but the declined line is %d units", got, want)
	}
}

// TestRetireReserveCountsTelegramUnits. Telegram counts UTF-16 code units, so a
// Chinese character is one against the 4096 budget where it is three bytes, and an
// emoji is two where it is one rune. Counting bytes over-reserves for every non-Latin
// script; counting runes under-reserves for the glyphs.
func TestRetireReserveCountsTelegramUnits(t *testing.T) {
	chinese := Question{Notes: OutcomeNotes{Dash: "——", Declined: "没有回答，视为拒绝", Withdrawn: "问题已撤回"}}
	got := retireReserve(chinese)
	if want := len(declinedText(chinese)); got >= want {
		t.Errorf("reserve = %d, which is at least the byte length %d — it is counting bytes", got, want)
	}
	if got <= 0 {
		t.Fatalf("reserve = %d", got)
	}
}

// TestOutcomeNotesFallBackPerString. A caller with no language gets English, and a
// caller that translated one line and not the other gets the line they translated
// rather than a table that degrades wholesale.
func TestOutcomeNotesFallBackPerString(t *testing.T) {
	q := Question{Text: "Q", Notes: OutcomeNotes{Declined: "sin respuesta"}}
	if got := declinedText(q); !strings.Contains(got, "sin respuesta") {
		t.Errorf("declined = %q, want the caller's own wording", got)
	}
	if got := withdrawnText(q); !strings.Contains(got, "question withdrawn") {
		t.Errorf("withdrawn = %q, want the English default for a line nobody translated", got)
	}
	if got := declinedText(Question{Text: "Q"}); !strings.Contains(got, "— no answer, treated as declined") {
		t.Errorf("a question with no notes at all rendered %q", got)
	}
}
