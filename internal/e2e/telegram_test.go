package e2e

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/transport/telegramtest"
)

// The household run over the real Telegram transport.
//
// Every other test in this package injects an Inbound straight into
// transport.Fake and reads an Outbound back out, which means the one component
// standing between the household and the outside world — the bot itself — is
// the only one nothing exercises. A bot token cannot be had, but Telegram's
// server can: telegramtest.Server speaks the Bot API over loopback, and
// transport.WithAPIServer is the seam the production code already carries for a
// self-hosted API server. So the transport here is the production one, byte for
// byte, and what it does is observed as HTTP requests rather than as method
// calls on a fake.
//
// What that buys over transport.Fake: real long polling with real getUpdates
// offsets, real multipart encoding, real JSON decoding, real inline keyboards
// rendered into reply_markup, and real callback_query updates coming back the
// other way. A capture confirmed here is confirmed by a button that was drawn on
// the wire and pressed on the wire.
//
// Below the transport this suite still fakes lore and the model, as the rest of
// the package does. That is deliberate rather than a compromise: live_test.go
// already runs the real ones, and its subject is retrieval and the store.
// The subject here is the wire, and pinning the model's decisions is what makes
// "the model proposed, the member pressed Personal, the write landed" a
// deterministic assertion about the transport instead of a coin toss about a
// 0.5b model's mood. These tests therefore run on a plain `go test ./...`, with
// no build tag, no endpoint and no binary — which matters, because a gap that
// only closes when two environment variables are set is a gap most of the time.

// telegramHousehold builds a household whose bot is the real transport.Telegram
// pointed at a local Bot API server, and returns both.
//
// The server is created first so that its cleanup runs last: cleanups are LIFO,
// so the transport is closed, then the supervisor stopped, and only then does
// the API server go away underneath them.
func telegramHousehold(t *testing.T, opts harnessOptions) (*harness, *telegramtest.Server) {
	t.Helper()
	api := telegramtest.New(t, testBotToken)
	opts.telegramAPI = api
	return newHarness(t, opts), api
}

// sendsTo returns every sendMessage the server received for one chat.
func sendsTo(api *telegramtest.Server, chatID int64) []telegramtest.Call {
	want := strconv.FormatInt(chatID, 10)
	var out []telegramtest.Call
	for _, c := range api.CallsFor("sendMessage") {
		if c.Form.Get("chat_id") == want {
			out = append(out, c)
		}
	}
	return out
}

// waitSends blocks until n messages have been sent to chatID over real HTTP.
func waitSends(t *testing.T, api *telegramtest.Server, chatID int64, n int) []telegramtest.Call {
	t.Helper()
	var got []telegramtest.Call
	waitFor(t, fmt.Sprintf("%d sendMessage call(s) to chat %d", n, chatID), func() bool {
		got = sendsTo(api, chatID)
		return len(got) >= n
	})
	return got
}

// waitKeyboard blocks until a message carrying an inline keyboard has been sent
// to chatID, and returns it. That message is the capture question: it is the
// only thing kenward ever sends with buttons on it.
func waitKeyboard(t *testing.T, api *telegramtest.Server, chatID int64) telegramtest.Call {
	t.Helper()
	var got telegramtest.Call
	waitFor(t, fmt.Sprintf("a message with a keyboard in chat %d", chatID), func() bool {
		for _, c := range sendsTo(api, chatID) {
			if c.Form.Get("reply_markup") != "" {
				got = c
				return true
			}
		}
		return false
	})
	return got
}

// buttonFor returns the callback_data behind the button labelled label, failing
// the test if the keyboard has no such button. Going through the label is the
// point: it is what the member sees and taps, and the callback_data is opaque to
// everyone but the transport that minted it.
func buttonFor(t *testing.T, rows [][]telegramtest.Button, label string) string {
	t.Helper()
	var seen []string
	for _, row := range rows {
		for _, b := range row {
			seen = append(seen, b.Text)
			if b.Text == label {
				return b.CallbackData
			}
		}
	}
	t.Fatalf("no button labelled %q; the keyboard offered %v", label, seen)
	return ""
}

// TestTelegramDirectMessageRoundTripsOverRealHTTP is the whole turn as Telegram
// would see it: an update arrives on a real getUpdates long poll, and the
// assistant's answer leaves as a real sendMessage.
func TestTelegramDirectMessageRoundTripsOverRealHTTP(t *testing.T) {
	h, api := telegramHousehold(t, harnessOptions{})
	h.mem.seed(davidSpace, entry("p1", "Bin day", "Recycling goes out on Tuesday night."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "Recycling goes out Tuesday night.", FinishReason: "stop"}
	})
	h.start()

	const asked = "when do the bins go out?"
	api.Push(telegramtest.TextUpdate(davidChatID, davidTelegramID, "private", asked))

	sent := waitSends(t, api, davidChatID, 1)
	if got := replyBody(sent[0].Form.Get("text")); got != "Recycling goes out Tuesday night." {
		t.Errorf("sendMessage carried text %q, want the model's reply", got)
	}
	// A direct chat is not quoted. The Fake could only ever show that Outbound
	// had ReplyTo zero; this shows that no reply_parameters was encoded at all.
	if got := sent[0].Form.Get("reply_parameters"); got != "" {
		t.Errorf("direct reply carried reply_parameters %q; direct chats do not quote", got)
	}

	// The member's own words came off the wire and reached the model.
	if got := h.local.last(t).UserText(); got != asked {
		t.Errorf("the model was asked %q, want the member's own words %q", got, asked)
	}
	if !strings.Contains(h.local.last(t).System(), "Recycling goes out on Tuesday night.") {
		t.Error("the system prompt does not carry what retrieval found")
	}
}

// TestTelegramPrivateWriteIsAnnouncedAndUndoneByARealButtonTap is the case with
// the most riding on it. A note to a member's own memory is written and then
// announced with an Undo button, and that button is the whole of what replaced
// the confirmation — so it has to be rendered onto a real wire and pressed from
// one, exactly as the confirmation was.
//
// Everything here is real except the model's decision and the store: the
// keyboard is encoded into reply_markup by the production transport, the
// callback_data is whatever that transport minted, the tap arrives as a
// callback_query on a real long poll, and the acknowledgement, the delete and the
// retirement edit go back out as real calls.
func TestTelegramPrivateWriteIsAnnouncedAndUndoneByARealButtonTap(t *testing.T) {
	h, api := telegramHousehold(t, harnessOptions{})
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{
			Text:         "The bins go out on Thursday.",
			FinishReason: "tool_calls",
			ToolCalls: []providerToolCall{{
				Name:      "remember",
				Arguments: rememberArgs("Boiler serviced", "The boiler was serviced in March.", "personal"),
			}},
		}
	})
	h.start()

	api.Push(telegramtest.TextUpdate(davidChatID, davidTelegramID, "private", "the boiler was serviced"))

	// The write comes first and the announcement follows it, so waiting on the
	// keyboard also proves the ordering: if the entry were not already stored,
	// there would be nothing here to offer to undo.
	announcement := waitKeyboard(t, api, davidChatID)
	if puts := h.mem.putCalls(); len(puts) != 1 {
		t.Fatalf("%d writes by the time the member was told, want exactly 1: %+v", len(puts), puts)
	}
	rows := telegramtest.Keyboard(t, announcement)
	// One button. There is nothing left to choose between — the entry is written
	// — so the keyboard offers the one thing still open.
	if len(rows) != 1 {
		t.Fatalf("keyboard has %d rows, want one: undo", len(rows))
	}
	undo := buttonFor(t, rows, "Undo")
	// The announcement carries the words that were stored rather than a summary
	// of them. The member never saw this text before it was written, so this
	// message is the only place it is ever shown.
	if got := announcement.Form.Get("text"); !strings.Contains(got, "The boiler was serviced in March.") {
		t.Errorf("announcement %q does not show what was written", got)
	}

	// Two taps, in order. The first is from another member of this very
	// household: in a group everyone can see a keyboard, and a callback_query is
	// only an update, so anyone who can reach the bot can post one. It must
	// change nothing at all. The second is from the member the question was
	// addressed to.
	//
	// Pushing both before waiting is what makes the negative assertion sound.
	// Updates are delivered in order, so once David's tap has been acted on,
	// Mei's has certainly been seen and dismissed — there is no window left in
	// which it might still be in flight.
	api.Push(telegramtest.CallbackUpdate("cb-mei", meiTelegramID, davidChatID, announcement.MessageID, undo))
	api.Push(telegramtest.CallbackUpdate("cb-david", davidTelegramID, davidChatID, announcement.MessageID, undo))

	// One tap settles the question, and settling it acknowledges the tap and
	// retires the message in that order. Waiting on the edit is therefore the
	// barrier that says a decision was made — before asking which tap made it.
	edit := api.WaitCall(t, "editMessageText", 1)
	ack := api.WaitCall(t, "answerCallbackQuery", 1)
	if got := ack.Form.Get("callback_query_id"); got != "cb-david" {
		t.Fatalf("the undo was taken from callback %q, want David's own tap; another member must not be able to delete out of his memory", got)
	}

	// The delete reaches the store, space-scoped, naming the space the entry was
	// written to and no other.
	waitFor(t, "the delete", func() bool { return len(h.mem.deleteCalls()) > 0 })
	dels := h.mem.deleteCalls()
	if len(dels) != 1 {
		t.Fatalf("%d deletes, want exactly 1: %+v", len(dels), dels)
	}
	if got := dels[0].Spaces; len(got) != 1 || got[0] != davidSpace {
		t.Errorf("deleted from %v, want the member's own space %s", got, davidSpace)
	}

	// The member is told the entry is gone, and told so only because it is — on the
	// message that said it was written, which is the one that stopped being true.
	// That is the second edit: the first retired the keyboard the moment the tap
	// landed, before anyone knew whether the delete would succeed.
	struck := api.WaitCall(t, "editMessageText", 2)

	// The spinner on the member's client is stopped, once and only for them.
	if n := api.CountFor("answerCallbackQuery"); n != 1 {
		t.Errorf("answerCallbackQuery called %d times, want 1; another member's tap must not be acknowledged, because an acknowledgement is itself a signal", n)
	}

	// Both edits land on the announcement, keyboard emptied, so an undo that has
	// happened cannot be tapped a second time.
	for i, c := range []telegramtest.Call{edit, struck} {
		if c.MessageID != announcement.MessageID {
			t.Errorf("edit %d hit message %d, want the announcement's own message %d", i+1, c.MessageID, announcement.MessageID)
		}
		if got := c.Form.Get("reply_markup"); !strings.Contains(got, `"inline_keyboard":[]`) {
			t.Errorf("keyboard not removed on edit %d: reply_markup = %q", i+1, got)
		}
	}
	if got := edit.Form.Get("text"); !strings.Contains(got, "The boiler was serviced in March.") || !strings.Contains(got, "Undo") {
		t.Errorf("retired announcement reads %q; it must keep what was written and show that undo was pressed", got)
	}
	// The rewritten one keeps the words and strikes them, and stops claiming they
	// are stored. This is what the chat still reads as a week later.
	got := struck.Form.Get("text")
	if !strings.Contains(got, "<s>The boiler was serviced in March.</s>") {
		t.Errorf("rewritten announcement %q does not show the struck entry", got)
	}
	if strings.Contains(got, "I've written this") {
		t.Errorf("rewritten announcement %q still says the entry was written", got)
	}
	if !strings.Contains(got, "not on any other device in the household") {
		t.Errorf("rewritten announcement %q drops the household-wide promise", got)
	}
	// And nothing is sent alongside it.
	for _, c := range sendsTo(api, davidChatID) {
		if strings.Contains(c.Form.Get("text"), "I've removed") {
			t.Errorf("a second removal notice was sent as well: %q", c.Form.Get("text"))
		}
	}
	if n := api.CountFor("editMessageText"); n != 2 {
		t.Errorf("editMessageText called %d times, want 2: the retirement and the rewrite", n)
	}
}

// TestTelegramGetUpdatesDeliversEachMessageOnce covers the offset handshake,
// which is where a long-polling client loses or repeats messages.
//
// Telegram does not consume an update when it hands it over — it keeps it until
// a later getUpdates asks for a higher offset. A client that never advanced its
// offset would be handed the same message forever and answer it forever, and
// nothing above the transport would notice: every turn would look correct on its
// own. So the assertion is on both sides. The server says each update was handed
// over once and then confirmed; the household says each message produced one
// completion and one reply.
func TestTelegramGetUpdatesDeliversEachMessageOnce(t *testing.T) {
	h, api := telegramHousehold(t, harnessOptions{})
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "The bins go out on Thursday.", FinishReason: "stop"}
	})
	h.start()

	first := api.Push(telegramtest.TextUpdate(davidChatID, davidTelegramID, "private", "one"))
	waitSends(t, api, davidChatID, 1)
	second := api.Push(telegramtest.TextUpdate(davidChatID, davidTelegramID, "private", "two"))
	waitSends(t, api, davidChatID, 2)

	// Let several more polls happen. If the offset were not advancing, each of
	// them would hand both updates over again and the household would answer them
	// again — which is the point of waiting rather than asserting immediately.
	polls := len(api.Offsets())
	waitFor(t, "three further polls", func() bool { return len(api.Offsets()) >= polls+3 })

	if n := api.Pending(); n != 0 {
		t.Errorf("%d update(s) still unconfirmed; the client never advanced its offset", n)
	}
	if n := api.Deliveries(first); n != 1 {
		t.Errorf("update %d was delivered %d times, want 1", first, n)
	}
	if n := api.Deliveries(second); n != 1 {
		t.Errorf("update %d was delivered %d times, want 1", second, n)
	}
	if n := h.local.count(); n != 2 {
		t.Errorf("the model was asked %d times for two messages, want 2", n)
	}
	if got := sendsTo(api, davidChatID); len(got) != 2 {
		t.Errorf("%d messages went out for two messages in, want 2", len(got))
	}

	offsets := api.Offsets()
	if last := offsets[len(offsets)-1]; last != second+1 {
		t.Errorf("the last poll asked for offset %d, want %d — one past the last update seen", last, second+1)
	}
}

// TestTelegramGroupMessageIsScopedToSharedMemory is the group invariant, checked
// with a real group chat on the wire rather than an IsGroup flag set by hand.
// The chat type arrives as "supergroup" in the update and nothing downstream is
// told anything else.
func TestTelegramGroupMessageIsScopedToSharedMemory(t *testing.T) {
	h, api := telegramHousehold(t, harnessOptions{})
	h.mem.seed(davidSpace, entry("p1", "David's PIN", "The card PIN is 9931."))
	h.mem.seed(meiSpace, entry("m1", "Mei's cardiologist", "Appointment on the 3rd."))
	h.mem.seed(sharedSpace, entry("s1", "Side gate", "The side gate code is 4417."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "The side gate code is 4417.", FinishReason: "stop"}
	})
	h.start()

	api.Push(telegramtest.MentionUpdate(groupChatID, davidTelegramID, "@kenward_bot what's the gate code?"))
	sent := waitSends(t, api, groupChatID, 1)

	for _, sp := range h.mem.touchedSpaces() {
		if sp != sharedSpace {
			t.Errorf("group turn touched space %s; a group scope may only ever reach %s", sp, sharedSpace)
		}
	}
	if searched := h.mem.searchedSpaces(); len(searched) != 1 {
		t.Errorf("group turn made %d space searches (%v), want exactly one", len(searched), searched)
	}
	if system := h.local.last(t).System(); strings.Contains(system, "9931") || strings.Contains(system, "Appointment on the 3rd.") {
		t.Error("the group prompt carries a private entry; the household chat must not see one")
	}

	// A group reply quotes the message it answers, so the household chat stays
	// legible when several turns overlap. Only the real transport shows this: it
	// is an encoded reply_parameters, not a struct field.
	if got := sent[0].Form.Get("reply_parameters"); got == "" {
		t.Error("group reply carried no reply_parameters; a group answer quotes the message it answers")
	}
	// Nothing private left the process on the Telegram wire either.
	for i, c := range api.CallsFor("sendMessage") {
		if strings.Contains(c.Form.Get("text"), "9931") {
			t.Errorf("sendMessage %d carries a private entry: %q", i, c.Form.Get("text"))
		}
	}
}

// TestTelegramGroupAnswersOnlyWhenAddressed is the household group rule end to
// end, over the wire that produces it.
//
// Telegram's bot privacy mode used to supply this gate: with it on a bot in a group
// receives only what names it. kenward now requires it off — with it on the bot
// received nothing at all and ignored a real family in silence — so the gate is
// rebuilt here, from the same entities Telegram would have filtered on.
//
// Both halves are asserted from one conversation, because they are one behaviour.
// The family talks; kenward says nothing and spends nothing. Then somebody asks it
// something, and what the family said is in the prompt — it was listening the whole
// time, which is the only reason not answering is acceptable.
func TestTelegramGroupAnswersOnlyWhenAddressed(t *testing.T) {
	h, api := telegramHousehold(t, harnessOptions{})
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "In the spring, you said.", FinishReason: "stop"}
	})
	h.start()

	const aside = "we said we would replace the boiler in the spring, not now"
	api.Push(telegramtest.TextUpdate(groupChatID, davidTelegramID, "supergroup", aside))
	api.Push(telegramtest.TextUpdate(groupChatID, meiTelegramID, "supergroup", "right, after the trip"))

	// A message that is answered, so there is something to wait for that is not a
	// sleep: it goes to a different chat and a different unit, and it cannot
	// overtake the group messages pushed before it — the transport polls one
	// stream in order.
	api.Push(telegramtest.TextUpdate(davidChatID, davidTelegramID, "private", "evening"))
	waitSends(t, api, davidChatID, 1)

	if got := sendsTo(api, groupChatID); len(got) != 0 {
		t.Fatalf("kenward answered %d messages the family said to each other: %q", len(got), got[0].Form.Get("text"))
	}
	if n := h.local.count(); n != 1 {
		t.Errorf("the model was asked %d times; want 1 — only the private message was a turn", n)
	}

	api.Push(telegramtest.MentionUpdate(groupChatID, davidTelegramID, "@kenward_bot what did we decide about the boiler?"))
	waitSends(t, api, groupChatID, 1)

	req := h.local.last(t)
	var heard bool
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, aside) {
			heard = true
		}
	}
	if !heard {
		t.Errorf("the messages kenward overheard never reached the prompt: %+v", req.Messages)
	}
	// Overheard messages are the conversation, not turns in it, so a run of them
	// arrives as one user message rather than as a run the chat template has to
	// cope with.
	for i := 1; i < len(req.Messages); i++ {
		if req.Messages[i].Role == req.Messages[i-1].Role {
			t.Errorf("two consecutive %s messages in the prompt: %+v", req.Messages[i].Role, req.Messages)
			break
		}
	}
}

// TestTelegramFailedPollNeitherLosesNorRepeatsAMessage is the failure case. A
// long-polling client meets a gateway that is having a bad minute, and the
// member has already sent their message by then.
//
// Telegram's contract is what makes this survivable: an update the client was
// never handed is still unconfirmed, so it comes back on the next successful
// poll. What must not happen is either half of the obvious failure — the message
// quietly disappearing, or the retry handing it over twice and the household
// answering the same question two times.
//
// The first poll is held open so that the failures and the member's message are
// certainly in place before anything is delivered; without that the ordering
// would be a race and the test would prove nothing in particular.
func TestTelegramFailedPollNeitherLosesNorRepeatsAMessage(t *testing.T) {
	h, api := telegramHousehold(t, harnessOptions{})
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "The bins go out on Thursday.", FinishReason: "stop"}
	})

	release := api.Hold("getUpdates")
	h.start()

	// A gateway error that is not JSON at all, then a well-formed API error.
	// Between them they cover both ways the client can fail to read a response.
	api.ScriptStatus("getUpdates", http.StatusBadGateway, "<html><body>502 Bad Gateway</body></html>")
	api.Script("getUpdates", `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
	id := api.Push(telegramtest.TextUpdate(davidChatID, davidTelegramID, "private", "did you get this?"))
	release()

	sent := waitSends(t, api, davidChatID, 1)
	if got := sent[0].Form.Get("text"); got != "The bins go out on Thursday." {
		t.Errorf("reply text = %q, want the model's", got)
	}

	// The two failures really happened, and the client asked for the same offset
	// each time: a failed poll confirms nothing, which is exactly why the message
	// survived it.
	var atFirstOffset int
	for _, o := range api.Offsets() {
		if o == id {
			atFirstOffset++
		}
	}
	if atFirstOffset < 3 {
		t.Errorf("offset %d was asked for %d times, want at least 3 (two failures and the poll that succeeded); offsets were %v",
			id, atFirstOffset, api.Offsets())
	}

	polls := len(api.Offsets())
	waitFor(t, "three further polls", func() bool { return len(api.Offsets()) >= polls+3 })

	if n := api.Deliveries(id); n != 1 {
		t.Errorf("the message was delivered %d times across the failures, want exactly 1", n)
	}
	if n := api.Pending(); n != 0 {
		t.Errorf("%d update(s) left unconfirmed after recovery", n)
	}
	if n := h.local.count(); n != 1 {
		t.Errorf("the model was asked %d times for one message, want 1", n)
	}
	if got := sendsTo(api, davidChatID); len(got) != 1 {
		t.Errorf("%d replies went out for one message, want 1", len(got))
	}
}

// TestTelegramSharedCaptureDeclinedByARealButtonTapWritesNothing is the rule that
// has no configuration flag, over the same wire: anything bound for the household
// is asked about first, every time, and a member who presses no leaves the shared
// memory untouched.
//
// A shared target rather than a personal one, deliberately. The personal path is
// the one that changed; this is the one that must not, and keeping a real-wire
// test on it is what stops the two being conflated again.
func TestTelegramSharedCaptureDeclinedByARealButtonTapWritesNothing(t *testing.T) {
	h, api := telegramHousehold(t, harnessOptions{})
	h.local.setReply(func(req wireRequest) providerReply {
		if strings.Contains(req.UserText(), "anything else") {
			return providerReply{Text: "Nothing else.", FinishReason: "stop"}
		}
		return providerReply{
			Text:         "The bins go out on Thursday.",
			FinishReason: "tool_calls",
			ToolCalls: []providerToolCall{{
				Name:      "remember",
				Arguments: rememberArgs("Boiler serviced", "The boiler was serviced in March.", "shared"),
			}},
		}
	})
	h.start()

	api.Push(telegramtest.TextUpdate(davidChatID, davidTelegramID, "private", "the boiler was serviced"))
	question := waitKeyboard(t, api, davidChatID)
	decline := buttonFor(t, telegramtest.Keyboard(t, question), "Don't save")

	api.Push(telegramtest.CallbackUpdate("cb-no", davidTelegramID, davidChatID, question.MessageID, decline))
	// The barrier: a unit serialises its turns, so the second message cannot be
	// answered until the first turn — capture included — has finished.
	api.Push(telegramtest.TextUpdate(davidChatID, davidTelegramID, "private", "anything else?"))
	waitFor(t, "the second turn to be answered", func() bool {
		for _, c := range sendsTo(api, davidChatID) {
			if replyBody(c.Form.Get("text")) == "Nothing else." {
				return true
			}
		}
		return false
	})

	if puts := h.mem.putCalls(); len(puts) != 0 {
		t.Errorf("a declined proposal wrote %+v; a decline must write nothing", puts)
	}
	edit := api.WaitCall(t, "editMessageText", 1)
	if got := edit.Form.Get("text"); !strings.Contains(got, "Don't save") {
		t.Errorf("retired question reads %q; it must show that the member declined", got)
	}
}

// TestTelegramMentionAndReplyRetrieveTheSame is the group gate's own defect, found
// in a live Spanish household and reproduced here over the wire that produced it.
//
// Since kenward stopped answering unaddressed group messages, an @mention is how a
// family addresses it. The handle then arrives inside the text — Telegram puts it
// nowhere else — and to a tokeniser "@kenward_bot" is words, not addressing. Six
// searchable words is all a turn gets, so the handle ate them, the shared memory was
// searched for the bot's own name, and the household was told its gate code had never
// been written down. Replying to kenward instead, with the same sentence, found it.
//
// Both halves are asserted from one conversation because the bug is the difference
// between them: the same question addressed two ways must reach the same memory.
//
// The searches are compared and not only the prompts, and that is deliberate. This
// server's bot is called @kenward_bot, which is two words to a tokeniser where the
// live household's was four, so two of six slots survived here and the entry came
// back anyway — the fault was visible in what was searched for before it was visible
// in what was found. A test that asserted only on the answer would go green on a
// household one word away from losing its memory.
func TestTelegramMentionAndReplyRetrieveTheSame(t *testing.T) {
	const question = "¿cuál es el código del portón del garaje?"

	h, api := telegramHousehold(t, harnessOptions{})
	h.mem.seed(sharedSpace, entry("s1",
		"Código del portón del garaje",
		"El código del portón del garaje es membrillo-3092."))
	h.local.setReply(func(wireRequest) providerReply {
		return providerReply{Text: "El código es membrillo-3092.", FinishReason: "stop"}
	})
	h.start()

	api.Push(telegramtest.MentionUpdate(groupChatID, davidTelegramID, "@kenward_bot "+question))
	waitSends(t, api, groupChatID, 1)
	mentioned, namedTerms := h.local.last(t).System(), searchedTerms(h.mem)

	// The barrier is the unit itself: it serialises its turns, so the reply turn
	// cannot be answered until the mention turn is finished and its prompt read.
	api.Push(telegramtest.ReplyUpdate(groupChatID, davidTelegramID, question))
	waitSends(t, api, groupChatID, 2)
	replied, allTerms := h.local.last(t).System(), searchedTerms(h.mem)
	repliedTerms := allTerms[len(namedTerms):]

	for _, tc := range []struct{ how, prompt string }{
		{"named", mentioned},
		{"replied to", replied},
	} {
		if !strings.Contains(tc.prompt, "membrillo-3092") {
			t.Errorf("a question %s kenward retrieved nothing: the shared entry is not in the prompt\n%s",
				tc.how, tc.prompt)
		}
	}

	slices.Sort(namedTerms)
	slices.Sort(repliedTerms)
	if !slices.Equal(namedTerms, repliedTerms) {
		t.Errorf("the same question searched the shared memory differently:\n named   %q\n replied %q",
			namedTerms, repliedTerms)
	}
	// And what it searched for is what was asked, rather than the name it was asked by.
	for _, term := range namedTerms {
		if !strings.Contains(strings.ToLower(question), term) {
			t.Errorf("the shared memory was searched for %q, which is not a word of %q", term, question)
		}
	}
}

// searchedTerms is every term one household has searched any space for, in order.
func searchedTerms(m *fakeMemory) []string {
	var out []string
	for _, c := range m.recorded() {
		if c.Op == "search" {
			out = append(out, c.Text)
		}
	}
	return out
}
