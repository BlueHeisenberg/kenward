package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/llm"

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/scope"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

func TestNewRejectsMissingDeps(t *testing.T) {
	base := func() Deps {
		mem := newFakeMemory()
		tr := &fakeTransport{}
		reminders, err := remind.Open("", testRemindOptions())
		if err != nil {
			t.Fatalf("opening an ephemeral reminder store: %v", err)
		}
		return Deps{
			Resolve:   fixedResolver(testDirectScope()),
			Memory:    mem,
			Router:    &fakeRouter{},
			Transport: tr,
			Sessions:  newFakeSessions(),
			Capture:   capture.New(mem, tr, capture.Options{}),
			Reminders: reminders,
		}
	}
	tests := []struct {
		name  string
		strip func(*Deps)
	}{
		{"resolve", func(d *Deps) { d.Resolve = nil }},
		{"memory", func(d *Deps) { d.Memory = nil }},
		{"router", func(d *Deps) { d.Router = nil }},
		{"transport", func(d *Deps) { d.Transport = nil }},
		{"sessions", func(d *Deps) { d.Sessions = nil }},
		{"capture", func(d *Deps) { d.Capture = nil }},
		{"reminders", func(d *Deps) { d.Reminders = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := base()
			tc.strip(&deps)
			if _, err := New(deps, Options{}); err == nil {
				t.Fatalf("New accepted Deps missing %s", tc.name)
			}
		})
	}
	if _, err := New(base(), Options{}); err != nil {
		t.Fatalf("New rejected complete Deps: %v", err)
	}
}

func TestHappyPathDirect(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["david-private"] = []memory.Entry{
		entry("david-private", "Coffee order", "David drinks oat-milk flat whites.", "validated"),
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		entry("household", "Bin day", "Bins go out Thursday night.", "hardened", "UPDATED"),
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "Thursday night.", Endpoint: "monster", Tier: "local"}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("when do the bins go out, and what is my coffee order?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Both spaces searched, and only those two.
	spaces := rig.mem.searchedSpaces()
	if len(spaces) != 2 {
		t.Fatalf("searched %d spaces, want 2: %v", len(spaces), spaces)
	}
	seen := map[domain.SpaceID]bool{}
	for _, s := range spaces {
		seen[s] = true
	}
	if !seen["david-private"] || !seen["household"] {
		t.Errorf("searched spaces %v, want both david-private and household", spaces)
	}

	// The reply reached the member.
	texts := rig.tr.sentTexts()
	if len(texts) != 1 || texts[0] != "Thursday night." {
		t.Errorf("sent %v, want exactly the reply", texts)
	}

	// The prompt carried both entries, grouped, and the chain was the scope's.
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("router never called")
	}
	sys := req.Messages[0].Content
	for _, want := range []string{
		"## From David's private memory",
		"- Coffee order [validated]",
		"## From the household's shared memory",
		"- Bin day [hardened] (UPDATED)",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if got := rig.router.chains[0]; len(got) != 1 || got[0] != "local" {
		t.Errorf("router chain %v, want [local]", got)
	}
	if got, want := toolNames(req.Tools), []string{"remember", "publish", "remind", "unremind"}; !slices.Equal(got, want) {
		t.Errorf("request tools %v, want %v", got, want)
	}

	// The session was touched, nothing was written, and the turn was recorded.
	if len(rig.sessions.touched) == 0 {
		t.Error("session never touched")
	}
	if rig.mem.putCount() != 0 {
		t.Error("memory written without a member confirmation")
	}
	if hist := rig.unit.history.snapshot(); len(hist) != 1 || hist[0].assistant != "Thursday night." {
		t.Errorf("history %v, want the delivered turn", hist)
	}
}

func TestGroupScopeNeverSearchesPrivateSpaces(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		entry("household", "Bin day", "Bins go out Thursday night.", "hardened"),
	}

	if err := rig.unit.Handle(context.Background(), groupInbound("bins?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	spaces := rig.mem.searchedSpaces()
	if len(spaces) != 1 || spaces[0] != "household" {
		t.Fatalf("group turn searched %v, want exactly [household]", spaces)
	}

	req, _ := rig.router.lastRequest()
	sys := req.Messages[0].Content
	if !strings.Contains(sys, "This is the Home group conversation.") {
		t.Error("group scope disclosure missing")
	}
	if strings.Contains(sys, "private conversation") {
		t.Error("direct disclosure leaked into a group prompt")
	}
	if got := strings.Count(sys, "## "); got != 1 {
		t.Errorf("group prompt has %d memory sections, want exactly the shared one", got)
	}
	if strings.Contains(sys, "'s private memory\n") {
		t.Error("a private memory section rendered in a group prompt")
	}
}

func TestUnknownSenderGetsAbsoluteSilence(t *testing.T) {
	rig, err := newTestRig(errResolver(scope.ErrNotEnrolled), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = rig.unit.Handle(context.Background(), directInbound("hello?"))
	if !errors.Is(err, scope.ErrNotEnrolled) {
		t.Fatalf("Handle returned %v, want ErrNotEnrolled", err)
	}
	if n := len(rig.tr.sentTexts()); n != 0 {
		t.Fatalf("sent %d messages to a stranger, want 0", n)
	}
	if rig.tr.askCount() != 0 {
		t.Fatal("asked a stranger a question")
	}
	if len(rig.mem.searchedSpaces()) != 0 {
		t.Fatal("searched memory for a stranger")
	}
}

func TestNoBackendBecomesRefusal(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{}, &routing.NoBackendError{
			Chain: []string{"local"},
			Tried: []string{"monster", "5090"},
		}
	}

	if err := rig.unit.Handle(context.Background(), directInbound("hi")); err != nil {
		t.Fatalf("a refusal is the product answering, not an error: %v", err)
	}

	texts := rig.tr.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("sent %d messages, want exactly the refusal", len(texts))
	}
	golden(t, "refusal_direct.golden", texts[0])

	if rig.mem.putCount() != 0 {
		t.Error("memory written on a refused turn")
	}
	if rig.tr.askCount() != 0 {
		t.Error("capture question asked on a refused turn")
	}
	if len(rig.unit.history.snapshot()) != 0 {
		t.Error("a refused turn was recorded in history")
	}
}

func TestLockedSessionPromptsAndStops(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.sessions.Lock("david")

	if err := rig.unit.Handle(context.Background(), directInbound("hi")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("sent %v, want exactly the locked prompt", texts)
	}
	golden(t, "locked.golden", texts[0])
	// The prompt must not invite the member to send anything: there is no unlock
	// flow over Telegram, deliberately, because a passphrase in a chat message is
	// a passphrase handed to Telegram and to the chat history.
	for _, word := range []string{"send", "reply", "passphrase", "message me", "ask me again"} {
		if strings.Contains(strings.ToLower(texts[0]), word) {
			t.Errorf("locked prompt contains %q, which invites a chat-based unlock", word)
		}
	}
	if len(rig.mem.searchedSpaces()) != 0 {
		t.Error("retrieval ran without a session")
	}
	if len(rig.router.chains) != 0 {
		t.Error("routing ran without a session")
	}
}

func TestContentFilterBecomesShortNotice(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text: "partial output that must not be shown",
			ToolCalls: []routing.ToolCall{{
				ID:        "tc-1",
				Name:      "remember",
				Arguments: json.RawMessage(`{"title": "T", "body": "B", "target": "shared"}`),
			}},
			FinishReason: routing.FinishContentFilter,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("hi")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("sent %v, want exactly the decline notice", texts)
	}
	golden(t, "content_filter.golden", texts[0])
	// A declined turn is final: no partial text, no capture, no history.
	if rig.tr.askCount() != 0 {
		t.Error("capture ran on a declined turn")
	}
	if rig.mem.putCount() != 0 {
		t.Error("memory written on a declined turn")
	}
	if len(rig.unit.history.snapshot()) != 0 {
		t.Error("a declined turn was recorded in history")
	}
}

func TestEmptyRetrievalRendersExplicitStatement(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.unit.Handle(context.Background(), directInbound("anything?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	req, _ := rig.router.lastRequest()
	sys := req.Messages[0].Content
	if got := strings.Count(sys, "(nothing relevant found)"); got != 2 {
		t.Errorf("empty-group statement appears %d times, want 2 (both spaces empty)\n%s", got, sys)
	}
	// Nothing was shown, so the paragraph saying entries are data rather than
	// instruction has nothing to describe and must not render.
	//
	// Asserted against the constant itself, not against a phrase inside it. A
	// substring is a proxy for the property and it drifts the other way round from
	// how a test should: rewording the prompt silences the guard, so the last person
	// to edit that paragraph had to write around the words this test happened to
	// pick. The constant is what renders, so the constant is what is asserted.
	if strings.Contains(sys, untrustedEntryNote) {
		t.Error("the untrusted-entry note rendered with no entries shown")
	}
}

// TestNaturalQuestionRetrievesTheEntryItIsAbout is the regression this package
// exists to keep. lore matches conjunctively over bare words, so a query is only
// found if every one of its words is in the entry; passing a member's raw message
// through as the query meant "what is the boiler service code?" retrieved nothing
// from a household that had recorded exactly that, and the assistant answered as
// though nothing had been stored. Every unit test missed it because the fake
// answered every query with everything it held.
func TestNaturalQuestionRetrievesTheEntryItIsAbout(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		entry("household", "Boiler", "The boiler service code is marlowbrick.", "validated"),
		entry("household", "Bin day", "Bins go out Thursday night.", "hardened"),
	}

	// Six filler words and two that matter, which is what a question looks like.
	if err := rig.unit.Handle(context.Background(), directInbound("sorry, do you happen to know what the code for the boiler is?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	sys := mustSystemPrompt(t, rig)
	if !strings.Contains(sys, "The boiler service code is marlowbrick.") {
		t.Errorf("the question did not retrieve the entry that answers it; system prompt was:\n%s", sys)
	}
	if strings.Contains(sys, "Bins go out Thursday night.") {
		t.Error("an unrelated entry was retrieved; the whole space is not the answer to a question")
	}

	// No query carried a filler word, and none carried more than one term: a
	// multi-word query is conjunctive, and one absent word empties the result.
	for _, q := range rig.mem.searches {
		if len(memory.Terms(q.Text)) != 1 {
			t.Errorf("search query %q is not a single term; lore ANDs them", q.Text)
		}
		if searchStopwords[q.Text] {
			t.Errorf("search query %q is a filler word; it costs a search and matches everything", q.Text)
		}
	}
}

// TestRetrievalRanksByHowManyTermsFoundAnEntry: the union is not a set, it is an
// ordering. An entry every content word found belongs above one that a single word
// brushed, or a budget-trimmed prompt drops the answer and keeps the noise.
func TestRetrievalRanksByHowManyTermsFoundAnEntry(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		// Seeded first, so first-seen order would put the weak hit on top.
		entry("household", "Gate", "The side gate sticks in the rain.", "provisional"),
		entry("household", "Gate code", "The side gate code is 4417.", "validated"),
	}

	if err := rig.unit.Handle(context.Background(), groupInbound("what is the side gate code?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	sys := mustSystemPrompt(t, rig)
	strong := strings.Index(sys, "The side gate code is 4417.")
	weak := strings.Index(sys, "The side gate sticks in the rain.")
	switch {
	case strong < 0:
		t.Fatal("the entry matching every term was not retrieved")
	case weak < 0:
		t.Fatal("the entry matching one term was not retrieved; retrieval is all-or-nothing again")
	case strong > weak:
		t.Error("the entry matching one term is rendered above the entry matching every term")
	}
}

// TestRetrievalKeepsAPreciseHitAheadOfACommonOne is the other half of ranking, and
// the half that counting terms gets wrong.
//
// A household accumulates entries about the same everyday things, and the per-term
// search budget is the same number the union is truncated to. So two ordinary words —
// "wifi", "password" — return a full budget of entries each, and the words that
// actually identify which one the member means are searched afterwards into a result
// that is already full. Counting terms ties the right entry with the wrong ones and
// breaks the tie on arrival order, which drops it. It is not a contrived shape: eight
// entries sharing one common word is a small household, and it was found against a
// real store.
func TestRetrievalKeepsAPreciseHitAheadOfACommonOne(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	var seeded []memory.Entry
	for _, place := range []string{"barn", "garage", "studio", "workshop", "cellar", "stables", "boathouse", "attic"} {
		seeded = append(seeded, entry("household", "Wifi password "+place,
			"The wifi password for the "+place+" is secret-"+place+".", "validated"))
	}
	// Seeded last, so it falls outside the budget every common word fills — which
	// is where lore's own relevance ordering would put it too.
	seeded = append(seeded, entry("household", "Wifi password guest cottage",
		"The wifi password for the guest cottage is secret-cottage.", "validated"))
	rig.mem.bySpace["household"] = seeded

	if err := rig.unit.Handle(context.Background(), groupInbound("what is the wifi password for the guest cottage?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	sys := mustSystemPrompt(t, rig)
	if !strings.Contains(sys, "secret-cottage") {
		t.Fatalf("the only entry the question identifies was crowded out by entries sharing its common words; system prompt was:\n%s", sys)
	}
	if at := strings.Index(sys, "secret-cottage"); at > strings.Index(sys, "secret-barn") {
		t.Error("an entry found only by the question's common words is rendered above the one found by its precise ones")
	}
}

// mustSystemPrompt returns the system prompt of the last request to reach the router.
func mustSystemPrompt(t *testing.T, rig *testRig) string {
	t.Helper()
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("router never called")
	}
	return req.Messages[0].Content
}

func TestMalformedRememberIsDroppedHarmlessly(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		// Prose, not "Noted." — a dropped call leaves nothing stored, so an
		// acknowledgement here would be replaced by the guard in Unit.turn and this
		// test would be measuring that instead of the parser. See
		// TestBareAcknowledgementWithNoCaptureIsNotSentAsIs.
		return routing.Completion{
			Text: "The bins go out on Thursday.",
			ToolCalls: []routing.ToolCall{
				{ID: "tc-1", Name: "remember", Arguments: json.RawMessage(`{this is not json`)},
			},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("remember this")); err != nil {
		t.Fatalf("a malformed tool call crashed the turn: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 || texts[0] != "The bins go out on Thursday." {
		t.Fatalf("sent %v, want just the reply", texts)
	}
	if rig.tr.askCount() != 0 {
		t.Error("a malformed proposal reached the member")
	}
	if rig.mem.putCount() != 0 {
		t.Error("a malformed proposal reached memory")
	}
}

func TestRememberProposalRunsCapture(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoicePersonal, UserID: testUserID}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text: "I'll remember that.",
			ToolCalls: []routing.ToolCall{{
				ID:        "tc-1",
				Name:      "remember",
				Arguments: json.RawMessage(`{"title": "Coffee order", "body": "David drinks oat-milk flat whites.", "target": "personal"}`),
			}},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("I only drink oat-milk flat whites")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := rig.tr.sentTexts()[0]; got != "I'll remember that." {
		t.Errorf("reply %q, want the model's text", got)
	}
	if rig.tr.askCount() != 1 {
		t.Fatalf("asked %d questions, want 1", rig.tr.askCount())
	}
	rig.mem.mu.Lock()
	defer rig.mem.mu.Unlock()
	if len(rig.mem.puts) != 1 {
		t.Fatalf("wrote %d entries, want 1 after the member confirmed", len(rig.mem.puts))
	}
	if p := rig.mem.puts[0]; p.space != "david-private" || p.draft.Title != "Coffee order" {
		t.Errorf("wrote %+v, want Coffee order in david-private", p)
	}
	if p := rig.mem.puts[0]; p.draft.Confidence != "provisional" {
		t.Errorf("confidence %q, want the provisional default", p.draft.Confidence)
	}
}

func TestCancellationSendsNothingAfterCtxDone(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		cancel()
		return routing.Completion{Text: "too late"}, nil
	}

	err = rig.unit.Handle(ctx, directInbound("hi"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle returned %v, want context.Canceled", err)
	}
	if n := len(rig.tr.sentTexts()); n != 0 {
		t.Fatalf("sent %d messages after cancellation, want 0", n)
	}
	if len(rig.unit.history.snapshot()) != 0 {
		t.Error("a cancelled turn was recorded in history")
	}
}

func TestConcurrentMessagesSerialise(t *testing.T) {
	opts := testOptions()
	opts.QueueLimit = 32
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	var inFlight, maxInFlight int
	var flightMu sync.Mutex
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		flightMu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		flightMu.Unlock()
		time.Sleep(2 * time.Millisecond)
		flightMu.Lock()
		inFlight--
		flightMu.Unlock()
		return routing.Completion{Text: "the bins go out on Thursday"}, nil
	}

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := directInbound("msg")
			in.MessageID = i + 1
			if err := rig.unit.Handle(context.Background(), in); err != nil {
				t.Errorf("Handle: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if maxInFlight != 1 {
		t.Fatalf("%d turns ran concurrently, want strict serialisation", maxInFlight)
	}
	if got := len(rig.tr.sentTexts()); got != n {
		t.Fatalf("sent %d replies, want %d", got, n)
	}
	if got := len(rig.unit.history.snapshot()); got != n {
		t.Fatalf("recorded %d turns, want %d", got, n)
	}
}

func TestQueueOverflowDropsWithNotice(t *testing.T) {
	opts := testOptions()
	opts.QueueLimit = 1
	opts.QueueNoticeAfter = time.Millisecond
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return routing.Completion{Text: "done"}, nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := rig.unit.Handle(context.Background(), directInbound("first")); err != nil {
			t.Errorf("first Handle: %v", err)
		}
	}()
	<-entered

	// Second message: queued, and told so once the notice grace expires.
	wg.Add(1)
	go func() {
		defer wg.Done()
		in := directInbound("second")
		in.MessageID = 2
		if err := rig.unit.Handle(context.Background(), in); err != nil {
			t.Errorf("second Handle: %v", err)
		}
	}()
	waitFor(t, func() bool {
		for _, txt := range rig.tr.sentTexts() {
			if txt == rig.unit.queued() {
				return true
			}
		}
		return false
	})

	// Third message: the queue is full; it is dropped, with a notice.
	in := directInbound("third")
	in.MessageID = 3
	if err := rig.unit.Handle(context.Background(), in); err != nil {
		t.Fatalf("third Handle: %v", err)
	}
	found := false
	for _, txt := range rig.tr.sentTexts() {
		if txt == enNotice(enCat.Dropped) {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropped message got no notice; sent: %v", rig.tr.sentTexts())
	}

	close(release)
	wg.Wait()

	// Exactly two turns actually ran.
	if got := len(rig.router.chains); got != 2 {
		t.Fatalf("%d turns ran, want 2 (third was dropped)", got)
	}
}

// TestContentFilterErrorFormBecomesNotice covers the common shape of a content
// filter: the endpoint sends finish_reason "content_filter" with empty content,
// which arrives from routing as an *llm.EmptyResponseError rather than a Completion.
// The member sees the same notice either way.
func TestContentFilterErrorFormBecomesNotice(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{}, fmt.Errorf("wrapped: %w", &llm.EmptyResponseError{
			Endpoint:     "monster",
			FinishReason: llm.FinishContentFilter,
			Detail:       "empty choice",
		})
	}

	if err := rig.unit.Handle(context.Background(), directInbound("hi")); err != nil {
		t.Fatalf("a decline is the product answering, not an error: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("sent %v, want exactly the decline notice", texts)
	}
	golden(t, "content_filter.golden", texts[0])
	if rig.tr.askCount() != 0 || rig.mem.putCount() != 0 {
		t.Error("a declined turn ran capture or wrote memory")
	}
	if len(rig.unit.history.snapshot()) != 0 {
		t.Error("a declined turn was recorded in history")
	}
}

// TestRouterFailuresGetReplies: a member who sends a message always gets a reply.
// Every router failure that is not a NoBackendError maps to a short classified
// notice; none of them may end in silence.
func TestRouterFailuresGetReplies(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		golden string
	}{
		{
			name:   "rate limited",
			err:    &llm.APIError{StatusCode: 429, Status: "429 Too Many Requests", Endpoint: "openrouter"},
			golden: "model_busy.golden",
		},
		{
			name:   "rotated key",
			err:    &llm.APIError{StatusCode: 401, Status: "401 Unauthorized", Endpoint: "openrouter"},
			golden: "misconfigured.golden",
		},
		{
			name:   "unknown model",
			err:    &llm.APIError{StatusCode: 404, Status: "404 Not Found", Endpoint: "monster"},
			golden: "misconfigured.golden",
		},
		{
			// A request the endpoint will not parse is permanent, whichever side
			// of the wire rejects it: retry advice would send the member back to
			// the same wall.
			name:   "rejected request",
			err:    &llm.APIError{StatusCode: 400, Status: "400 Bad Request", Endpoint: "monster"},
			golden: "misconfigured.golden",
		},
		{
			name:   "invalid request",
			err:    fmt.Errorf("building request: %w", llm.ErrInvalidRequest),
			golden: "misconfigured.golden",
		},
		{
			name:   "unclassified failure",
			err:    errors.New("routing: endpoint monster: environment variable MONSTER_KEY is not set"),
			golden: "turn_failed.golden",
		},
		{
			name:   "empty response without a finish reason",
			err:    &llm.EmptyResponseError{Endpoint: "monster", Detail: "no choices"},
			golden: "turn_failed.golden",
		},
		{
			// A reasoning model that thought for the whole turn and answered
			// nothing. Routing declines to fail over on it, so it arrives here
			// instead of as a refusal naming machines that were never at fault,
			// and the member is told what actually happened rather than that
			// their household has no reachable machine.
			name: "reasoning only",
			err: &llm.EmptyResponseError{
				Endpoint:     "monster",
				FinishReason: llm.FinishLength,
				Detail:       llm.DetailReasoningOnly,
				Reasoning:    "Okay, the user wants annual appliance energy costs.",
			},
			golden: "reasoning_only.golden",
		},
		{
			// The finish reason must not be the discriminator: measured, the
			// same endpoint returns null content under "stop" with a third of
			// the budget unspent. Reasoning is what identifies the case.
			name: "reasoning only claiming a normal stop",
			err: &llm.EmptyResponseError{
				Endpoint:     "monster",
				FinishReason: llm.FinishStop,
				Detail:       llm.DetailReasoningOnly,
				Reasoning:    "Okay, the user wants annual appliance energy costs.",
			},
			golden: "reasoning_only.golden",
		},
		{
			// A model that reasoned its way to declining still declined, and
			// the decline is the more important thing to say.
			name: "content filter that also carried a trace",
			err: &llm.EmptyResponseError{
				Endpoint:     "monster",
				FinishReason: llm.FinishContentFilter,
				Detail:       llm.DetailReasoningOnly,
				Reasoning:    "This asks me to do something I won't.",
			},
			golden: "content_filter.golden",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
			if err != nil {
				t.Fatal(err)
			}
			rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
				return routing.Completion{}, tc.err
			}
			if err := rig.unit.Handle(context.Background(), directInbound("hi")); err != nil {
				t.Fatalf("the notice is the product answering, not an error: %v", err)
			}
			texts := rig.tr.sentTexts()
			if len(texts) != 1 {
				t.Fatalf("sent %v, want exactly one notice — silence is the one wrong answer", texts)
			}
			golden(t, tc.golden, texts[0])
			if len(rig.unit.history.snapshot()) != 0 {
				t.Error("a failed turn was recorded in history")
			}
		})
	}
}

// TestPutFailureReachesMember tests the join the packages' own tests miss: the
// member taps Save, the store fails, and the member — not just the log — hears
// that nothing was stored.
//
// It used to assert a hedge, because a lost MCP response left a write that might
// have landed under an id kenward never received. The store commits or returns
// now, so the member gets the fact.
func TestPutFailureReachesMember(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.putErr = errors.New("lore is down")
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoicePersonal, UserID: testUserID}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text: "I'll remember that.",
			ToolCalls: []routing.ToolCall{{
				ID:        "tc-1",
				Name:      "remember",
				Arguments: json.RawMessage(`{"title": "Coffee order", "body": "Oat milk.", "target": "personal"}`),
			}},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("remember this")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var told bool
	for _, txt := range rig.tr.sentTexts() {
		if strings.Contains(txt, "couldn't save") && strings.Contains(txt, "nothing was stored") {
			told = true
		}
		if strings.Contains(txt, "Saved") {
			t.Errorf("member saw a confirmation for a write that failed: %q", txt)
		}
	}
	if !told {
		t.Fatalf("member never told the write failed; sent: %v", rig.tr.sentTexts())
	}
}

// TestCaptureQuestionDoesNotBlockNextTurn: the turn slot covers the node's work, not
// the member's tap. A member who ignores the buttons and asks something else gets an
// answer, not a queue notice.
func TestCaptureQuestionDoesNotBlockNextTurn(t *testing.T) {
	opts := testOptions()
	opts.QueueNoticeAfter = time.Millisecond
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	rig.tr.askGate = gate
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoicePersonal, UserID: testUserID}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		last := req.Messages[len(req.Messages)-1].Content
		if last == "remember this" {
			return routing.Completion{
				Text: "Noted.",
				ToolCalls: []routing.ToolCall{{
					ID:        "tc-1",
					Name:      "remember",
					Arguments: json.RawMessage(`{"title": "T", "body": "B", "target": "personal"}`),
				}},
				FinishReason: routing.FinishToolCalls,
			}, nil
		}
		return routing.Completion{Text: "Thursday."}, nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := rig.unit.Handle(context.Background(), directInbound("remember this")); err != nil {
			t.Errorf("first Handle: %v", err)
		}
	}()
	waitFor(t, func() bool { return rig.tr.askCount() == 1 })

	// The question is open and unanswered. The next message must run as a normal
	// turn: answered, and never told it was queued.
	in := directInbound("when do the bins go out?")
	in.MessageID = 2
	if err := rig.unit.Handle(context.Background(), in); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	var answered bool
	for _, txt := range rig.tr.sentTexts() {
		if txt == "Thursday." {
			answered = true
		}
		if txt == enNotice(enCat.Queued) || txt == enNotice(enCat.Dropped) {
			t.Errorf("member blamed for a turn that was waiting on their own tap: %q", txt)
		}
	}
	if !answered {
		t.Fatalf("second message never answered while a question was open; sent: %v", rig.tr.sentTexts())
	}

	close(gate)
	wg.Wait()
	if rig.mem.putCount() != 1 {
		t.Errorf("puts = %d, want the answered question's write", rig.mem.putCount())
	}
}

// TestToolCallOnlyTurnIsNotRecorded: a turn whose reply was empty must not leave an
// empty assistant side in history — two consecutive user messages break several
// local chat templates.
func TestToolCallOnlyTurnIsNotRecorded(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoiceDecline, UserID: testUserID}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			ToolCalls: []routing.ToolCall{{
				ID:        "tc-1",
				Name:      "remember",
				Arguments: json.RawMessage(`{"title": "T", "body": "B", "target": "personal"}`),
			}},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("remember this")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(rig.unit.history.snapshot()); got != 0 {
		t.Fatalf("history holds %d turns, want 0 for a turn with no assistant side", got)
	}
}

// TestEmptyCompletionGetsNotice: a completion with no text and no tool call is not a
// content filter and not a router error, so nothing else in the turn speaks for it.
// The member still gets an answer — silence is the one wrong response.
func TestEmptyCompletionGetsNotice(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{FinishReason: routing.FinishStop, Endpoint: "monster", Tier: "local"}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("hi")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("sent %v, want exactly the empty-turn notice", texts)
	}
	golden(t, "no_answer.golden", texts[0])
	if len(rig.unit.history.snapshot()) != 0 {
		t.Error("a turn with no reply was recorded in history")
	}
}

// TestBareAcknowledgementWithNoCaptureIsNotSentAsIs is the release blocker, in a test.
//
// A live English run put ten "remember this for me: …" messages to a real model on a
// fresh store. Eight produced a memory card. Two produced "Done." and "Got it.", and a
// separate lore process confirmed exactly eight entries. The member asked outright, was
// told the job was done, and nothing exists — which is D-059 arriving on the one path
// where the intent is unambiguous, and arriving without a verb for the prompt's
// prohibition to catch.
//
// Whether the model calls the tool is the model's judgement and is measured, not
// asserted, in judgement_eval_test.go. What is asserted here is the half that is
// kenward's: on a turn where no remember call arrived, a reply that says nothing but
// "Done." never reaches the member as it stands.
func TestBareAcknowledgementWithNoCaptureIsNotSentAsIs(t *testing.T) {
	for _, body := range []string{"Done.", "Got it.", "Noted!", "OK 👍"} {
		t.Run(body, func(t *testing.T) {
			rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
			if err != nil {
				t.Fatal(err)
			}
			rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
				return routing.Completion{Text: body, FinishReason: routing.FinishStop}, nil
			}

			in := directInbound("remember this for me: I always buy the tarragon-brand coffee beans")
			if err := rig.unit.Handle(context.Background(), in); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			texts := rig.tr.sentTexts()
			if len(texts) != 1 {
				t.Fatalf("sent %v, want exactly one message", texts)
			}
			if strings.Contains(texts[0], body) {
				t.Errorf("the member was sent %q on a turn where nothing was stored: an acknowledgement is a claim that something happened, and nothing did", texts[0])
			}
			if !strings.Contains(texts[0], lang.For("").NothingSaved) {
				t.Errorf("the member was sent %q, which never says nothing was recorded", texts[0])
			}
			if rig.mem.putCount() != 0 {
				t.Error("something was written to memory on a turn the model never proposed one; the fix is a notice, never a retry")
			}
			if got := len(rig.unit.history.snapshot()); got != 0 {
				t.Errorf("history holds %d turns: a dropped acknowledgement must not be fed back as the assistant's words", got)
			}
		})
	}
}

// TestBareAcknowledgementIsReplacedInTheMembersOwnLanguage. The product speaks ten
// languages and a member writing "apunta esto" is owed the same protection as one
// writing "remember this". The signal is the reply rather than the message precisely
// so that this costs nothing: the acknowledgement is written in the member's language
// because the persona told the model to write in it, and lang is the one package that
// knows ten of those.
func TestBareAcknowledgementIsReplacedInTheMembersOwnLanguage(t *testing.T) {
	opts := testOptions()
	opts.Persona = Persona{Language: "español"}
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "¡Hecho!", FinishReason: routing.FinishStop}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("apunta esto: siempre compro café de la marca Estragón")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 || !strings.Contains(texts[0], lang.For("es").NothingSaved) {
		t.Fatalf("sent %v, want the Spanish notice that nothing was recorded", texts)
	}
}

// TestAnAcknowledgementCarryingAnAnswerIsLeftAlone is where the line is drawn, and why
// dropping the reply is safe.
//
// Only a reply that is *nothing but* an acknowledgement is replaced, so a reply that
// carries anything a member could act on cannot be lost. "Done — the code is 4471"
// keeps the code. The named false positive — "remember when we went to Lisbon?" — never
// reaches the guard at all, because the answer to it is a sentence about Lisbon.
func TestAnAcknowledgementCarryingAnAnswerIsLeftAlone(t *testing.T) {
	for _, body := range []string{
		"Done — the boiler service code is 4471.",
		"We went in the spring of 2019, four nights, and you got sunburnt.",
		"Yes.",
	} {
		t.Run(body, func(t *testing.T) {
			rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
			if err != nil {
				t.Fatal(err)
			}
			rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
				return routing.Completion{Text: body, FinishReason: routing.FinishStop}, nil
			}

			if err := rig.unit.Handle(context.Background(), directInbound("remember when we went to Lisbon?")); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			texts := rig.tr.sentTexts()
			if len(texts) != 1 || texts[0] != body {
				t.Fatalf("sent %v, want the model's reply untouched: a reply that carries information is not a bare acknowledgement", texts)
			}
		})
	}
}

// TestAcknowledgementSurvivesWhenTheTurnActuallyDidSomething. "Done." is true on a turn
// that proposed a capture, set a reminder or published an entry — the node did act, and
// the capture engine or the reminder notice says so in its own words. The guard reads
// what this turn did and not what the member typed, so it must not fire on any of them.
func TestAcknowledgementSurvivesWhenTheTurnActuallyDidSomething(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoiceDecline, UserID: testUserID}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text: "Done.",
			ToolCalls: []routing.ToolCall{{
				ID:        "tc-1",
				Name:      "remember",
				Arguments: json.RawMessage(`{"title": "Coffee", "body": "Tarragon-brand beans.", "target": "unsure"}`),
			}},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("remember this for me: tarragon-brand beans")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 || texts[0] != "Done." {
		t.Fatalf("sent %v, want the reply untouched on a turn that did propose a capture", texts)
	}
}

// TestSuppressedProposalOnBareToolCallGetsNotice: the second silence path. The model
// says nothing and only calls the tool, and the capture engine suppresses the
// proposal as a duplicate — so it asks nothing either, and without a notice the
// member's message produces no message at all.
func TestSuppressedProposalOnBareToolCallGetsNotice(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoiceDecline, UserID: testUserID}
	// An unsure target, because this test needs a question to decline and unsure is
	// the target that is always put as one. A personal target is written and
	// announced instead, which is a different path with its own tests in
	// internal/capture; what is under test here is the notice on a turn that ends up
	// saying nothing at all.
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			ToolCalls: []routing.ToolCall{{
				ID:        "tc-1",
				Name:      "remember",
				Arguments: json.RawMessage(`{"title": "Coffee order", "body": "Oat milk.", "target": "unsure"}`),
			}},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	// First turn: the member is asked and declines. They saw a question, so nothing
	// further is owed to them.
	if err := rig.unit.Handle(context.Background(), directInbound("remember this")); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if got := len(rig.tr.sentTexts()); got != 0 {
		t.Fatalf("sent %v after a declined question, want nothing", rig.tr.sentTexts())
	}

	// Second turn: the same title, now suppressed as a duplicate without asking.
	in := directInbound("remember this")
	in.MessageID = 2
	if err := rig.unit.Handle(context.Background(), in); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if rig.tr.askCount() != 1 {
		t.Fatalf("asked %d questions, want 1 — the duplicate must not be asked again", rig.tr.askCount())
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("sent %v, want exactly the empty-turn notice", texts)
	}
	golden(t, "no_answer.golden", texts[0])
	if rig.mem.putCount() != 0 {
		t.Error("memory written on a suppressed proposal")
	}
}

// publishCompletion is a bare publish call for the named title.
func publishCompletion(title string) routing.Completion {
	return routing.Completion{
		Text: "Here it is.",
		ToolCalls: []routing.ToolCall{{
			ID:        "tc-1",
			Name:      publishToolName,
			Arguments: json.RawMessage(fmt.Sprintf(`{"title": %q}`, title)),
		}},
		FinishReason: routing.FinishToolCalls,
	}
}

// TestPublishGoesThroughShareWithARetrievedID: the promotion flow, end to end. The
// member asks, the model names a title it can see, and the id handed to memory is the
// one this turn's search returned — never one the model or the member wrote.
func TestPublishGoesThroughShareWithARetrievedID(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["david-private"] = []memory.Entry{
		entry("david-private", "Dentist", "Appointment on the first Monday of October.", "validated"),
	}
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoicePublish, UserID: testUserID}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		// The tool is offered here, and its schema takes no id.
		var found bool
		for _, spec := range req.Tools {
			if spec.Name == publishToolName {
				found = true
				if strings.Contains(string(spec.Schema), `"id"`) {
					t.Error("the publish schema accepts an id; ids may not come from the model")
				}
			}
		}
		if !found {
			t.Error("publish tool not offered in a direct scope")
		}
		// The model copies the title back out of the prompt, cosmetics and all.
		return publishCompletion("  dentist  "), nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("publish my dentist note")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	shares := rig.mem.sharedCalls()
	if len(shares) != 1 {
		t.Fatalf("Share called %d times, want 1: %+v", len(shares), shares)
	}
	if got := shares[0]; got.from != "david-private" || got.to != "household" || got.entryID != "id-Dentist" {
		t.Errorf("shared %+v, want id-Dentist from david-private to household", got)
	}
	// Share, never a read-then-put: lore's provenance survives a promotion.
	if rig.mem.putCount() != 0 {
		t.Error("promotion wrote a new entry instead of sharing the existing one")
	}
	// The member saw the full text before deciding.
	if rig.tr.askCount() != 1 {
		t.Fatalf("asked %d questions, want the publication preview", rig.tr.askCount())
	}
	rig.tr.mu.Lock()
	preview := rig.tr.asked[0].Text
	rig.tr.mu.Unlock()
	if !strings.Contains(preview, "Appointment on the first Monday of October.") {
		t.Errorf("preview %q does not show the entry's full text", preview)
	}
	if !strings.Contains(preview, "cannot be unpublished") {
		t.Errorf("preview %q does not say publication is irreversible", preview)
	}
}

// barePublishCompletion is a publish call with no prose beside it — which is what
// publishText asks the model for, and therefore the shape a real publish turn has.
func barePublishCompletion(title string) routing.Completion {
	c := publishCompletion(title)
	c.Text = ""
	return c
}

// dentistNote seeds the private space with the entry both publish-failure tests
// resolve a title against.
func dentistNote(rig *testRig) {
	rig.mem.bySpace["david-private"] = []memory.Entry{
		entry("david-private", "Dentist", "Appointment on the first Monday of October.", "validated"),
	}
}

// TestPublishReadFailureStillAnswers: the whole turn is a bare publish call, the id
// resolves out of this turn's own search, and lore then fails the read-back that
// builds the preview. No question is asked and nothing is published — so unless the
// node says so, the member's message produces no message at all. Silence is the one
// wrong answer (IMPLEMENTATION.md section 10).
func TestPublishReadFailureStillAnswers(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	dentistNote(rig)
	rig.mem.getErr = errors.New("lore: deadline exceeded")
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return barePublishCompletion("Dentist"), nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("publish my dentist note")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if rig.tr.askCount() != 0 {
		t.Errorf("asked %d questions about an entry that could not be read", rig.tr.askCount())
	}
	texts := rig.tr.sentTexts()
	if len(texts) == 0 {
		t.Fatal("the member's turn ended in total silence")
	}
	if len(texts) != 1 {
		t.Fatalf("sent %v, want exactly one notice", texts)
	}
	if strings.Contains(texts[0], "Published") {
		t.Errorf("notice %q claims a publication that did not happen", texts[0])
	}
}

// TestPublishShareFailureIsReported: the worse half. The member saw the full text,
// tapped Publish to household — an act that cannot be taken back — and the store
// then failed. They are told that nothing reached the household, which is now a
// fact rather than the hedge it had to be while a lost reply could leave a private
// entry in the shared space with nobody able to name it.
func TestPublishShareFailureIsReported(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	dentistNote(rig)
	rig.mem.shareErr = errors.New("lore is down")
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoicePublish, UserID: testUserID}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return publishCompletion("Dentist"), nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("publish my dentist note")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) < 2 {
		t.Fatalf("sent %v, want the reply and a notice about the publication", texts)
	}
	got := texts[len(texts)-1]
	if !strings.Contains(got, "couldn't publish") || !strings.Contains(got, "nothing reached the household") {
		t.Errorf("notice %q does not tell the member the publication did not happen", got)
	}
	if strings.Contains(got, "Everyone can see it now") {
		t.Errorf("notice %q reads as a confirmation", got)
	}
	if got == enNotice(enCat.NoAnswer) {
		t.Errorf("the member authorised a publication and was told %q", got)
	}
}

// TestPublishIDNeverComesFromModelText is the provenance rule as an assertion: a
// publish call for a title this turn's search did not return writes nothing, asks
// nothing, and does not reach memory at all. lore's ids are global and lore_get is
// not space-scoped, so an id the node did not retrieve itself is an id it cannot
// vouch for — and the model is where member text arrives.
func TestPublishIDNeverComesFromModelText(t *testing.T) {
	tests := []struct {
		name  string
		title string
	}{
		{"a title that was never retrieved", "Someone else's private note"},
		{"a raw lore id passed off as a title", "id-Dentist"},
		{"a title retrieved from the shared space, not the private one", "Bin day"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
			if err != nil {
				t.Fatal(err)
			}
			rig.mem.bySpace["david-private"] = []memory.Entry{
				entry("david-private", "Dentist", "Appointment in October.", "validated"),
			}
			rig.mem.bySpace["household"] = []memory.Entry{
				entry("household", "Bin day", "Bins go out Thursday night.", "hardened"),
			}
			rig.tr.answer = transport.Answer{ChoiceID: capture.ChoicePublish, UserID: testUserID}
			rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
				return publishCompletion(tc.title), nil
			}

			if err := rig.unit.Handle(context.Background(), directInbound("publish that")); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			// The id never reaches memory at all — not Share, and not the Get
			// behind the preview. Anything else would mean the node acted on an id
			// it did not retrieve itself.
			if got := rig.mem.gotIDs(); len(got) != 0 {
				t.Errorf("memory read with %v; an unretrieved id reached the store", got)
			}
			if got := rig.mem.sharedCalls(); len(got) != 0 {
				t.Errorf("Share reached with %+v; the id did not come from this scope's search", got)
			}
			if rig.tr.askCount() != 0 {
				t.Error("a member was asked to confirm publishing an entry this turn never retrieved")
			}
			if rig.mem.putCount() != 0 {
				t.Error("memory written on a dropped publish call")
			}
			// The turn still answered: the reply carried it.
			if texts := rig.tr.sentTexts(); len(texts) != 1 || texts[0] != "Here it is." {
				t.Errorf("sent %v, want just the reply", texts)
			}
		})
	}
}

// TestGroupScopeIsNotOfferedPublish: publishing from the household group is
// meaningless — the entry is already there — so the tool is not offered, and a model
// that calls it anyway is refused by the scope, not by luck.
func TestGroupScopeIsNotOfferedPublish(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		entry("household", "Bin day", "Bins go out Thursday night.", "hardened"),
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		for _, spec := range req.Tools {
			if spec.Name == publishToolName {
				t.Error("publish tool offered in a group scope")
			}
		}
		return publishCompletion("Bin day"), nil
	}

	if err := rig.unit.Handle(context.Background(), groupInbound("publish the bin day note")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := rig.mem.sharedCalls(); len(got) != 0 {
		t.Errorf("a group turn reached Share: %+v", got)
	}
	if rig.tr.askCount() != 0 {
		t.Error("a group turn asked a publication question")
	}
}

// TestRepeatedMessageIDStillCaptures: turn tokens are unique per turn even when the
// transport repeats a message id, so the capture budget is fresh each turn.
func TestRepeatedMessageIDStillCaptures(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.tr.answer = transport.Answer{ChoiceID: capture.ChoicePersonal, UserID: testUserID}
	titles := []string{"First thing", "Second thing"}
	turn := 0
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		title := titles[turn]
		turn++
		return routing.Completion{
			Text: "Noted.",
			ToolCalls: []routing.ToolCall{{
				ID:        "tc-1",
				Name:      "remember",
				Arguments: json.RawMessage(fmt.Sprintf(`{"title": %q, "body": "B", "target": "personal"}`, title)),
			}},
			FinishReason: routing.FinishToolCalls,
		}, nil
	}

	for i := 0; i < 2; i++ {
		in := directInbound("remember this")
		in.MessageID = 0 // a degenerate transport that never numbers messages
		if err := rig.unit.Handle(context.Background(), in); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if got := rig.tr.askCount(); got != 2 {
		t.Fatalf("asked %d questions, want 2 — a repeated turn token spent the budget", got)
	}
	if got := rig.mem.putCount(); got != 2 {
		t.Fatalf("puts = %d, want 2", got)
	}
}

// TestNewRejectsReservationLargerThanBudget: a completion reservation the context
// budget cannot hold is a configuration contradiction, refused at construction
// rather than silently unreserved at assembly.
func TestNewRejectsReservationLargerThanBudget(t *testing.T) {
	mem := newFakeMemory()
	tr := &fakeTransport{}
	reminders, err := remind.Open("", testRemindOptions())
	if err != nil {
		t.Fatalf("opening an ephemeral reminder store: %v", err)
	}
	deps := Deps{
		Resolve:   fixedResolver(testDirectScope()),
		Memory:    mem,
		Router:    &fakeRouter{},
		Transport: tr,
		Sessions:  newFakeSessions(),
		Capture:   capture.New(mem, tr, capture.Options{}),
		Reminders: reminders,
	}
	if _, err := New(deps, Options{ContextBudget: 2048, MaxTokens: 2048}); err == nil {
		t.Fatal("New accepted MaxTokens == ContextBudget")
	}
	if _, err := New(deps, Options{ContextBudget: 2048, MaxTokens: 4096}); err == nil {
		t.Fatal("New accepted MaxTokens > ContextBudget")
	}
	if _, err := New(deps, Options{ContextBudget: 2048, MaxTokens: 512}); err != nil {
		t.Fatalf("New rejected a valid reservation: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// --- the retrieval line ------------------------------------------------------

// TestReplyCarriesTheRetrievalLine: the member is told what was read, and told it on
// the reply rather than in a message of its own.
//
// The second half is the part worth pinning. A turn already costs a reply and may cost
// a write announcement; a third message on every turn — most of them reporting that
// nothing was found — is how a household learns to stop reading any of them. One
// message out is the whole design decision, and it is invisible in the text.
func TestReplyCarriesTheRetrievalLine(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["david-private"] = []memory.Entry{
		entry("david-private", "Boiler service", "Serviced in March.", "validated"),
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		entry("household", "Boiler code", "The code is 4417.", "validated"),
		entry("household", "Boiler engineer", "Boiler engineer is Ravi.", "validated"),
	}
	rig.router.fn = func(context.Context, []string, routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "It was serviced in March."}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("boiler")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	texts := rig.tr.sentTextsRaw()
	if len(texts) != 1 {
		t.Fatalf("sent %v, want one message carrying both the line and the reply", texts)
	}
	want := "<i>🔍 searched your private memory (1 entry), the household memory (2 entries)</i>\n\nIt was serviced in March."
	if texts[0] != want {
		t.Errorf("sent\n  %q\nwant\n  %q", texts[0], want)
	}
}

// TestRetrievalLineReportsWhatReachedTheModel: the numbers are counted after the
// budget loop, not off the search results.
//
// Retrieval finding eight entries and the prompt carrying two is an ordinary
// occurrence on a small endpoint, and a line claiming eight informed the answer would
// be a statement about the answer's basis rather than about retrieval. The prompt
// itself already admits the drop to the model; this is the member's half of the same
// admission.
func TestRetrievalLineReportsWhatReachedTheModel(t *testing.T) {
	opts := testOptions()
	// Room for the prompt and a little else, so the budget loop has to drop some of
	// the shared group but not all of it. The number tracks the size of the fixed
	// prompt: it was raised when the reminders section was added, because with the
	// old budget only one entry survived and the assertion below needs a plural, and
	// again when the capture block gained the paragraph saying a tool call is not a
	// write and the identity section gained the plain-prose rule, which between them
	// left no room for any entry at all.
	opts.ContextBudget = 2000
	opts.MaxTokens = 128
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		rig.mem.bySpace["household"] = append(rig.mem.bySpace["household"],
			entry("household", fmt.Sprintf("Boiler note %d", i),
				strings.Repeat("Boiler detail that takes room. ", 12), "validated"))
	}
	rig.router.fn = func(context.Context, []string, routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "Answered."}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("boiler")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	line, _, _ := strings.Cut(rig.tr.sentTextsRaw()[0], "\n")
	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("no request reached the router")
	}
	// entryOpen also appears in the prompt's own note about what an entry is,
	// so the count is anchored on the bullet line that only a rendered entry has.
	shown := strings.Count(req.Messages[0].Content, entryOpen+"\n- ")
	if shown == 0 || shown == 8 {
		t.Fatalf("the budget dropped %d of 8 entries; this test needs some dropped and some kept", 8-shown)
	}
	// Built with the catalogue rather than a format string, so the assertion survives
	// the budget leaving exactly one entry. It used to spell the count itself and
	// always pluralised, which meant a prompt three lines longer — enough to drop
	// one more entry — failed this test with "(1 entry)" against a wanted
	// "(1 entries)": a fault in the expectation, reported as a fault in the code.
	if want := enCat.PartShared(enCat.Count(false, shown)); !strings.Contains(line, want) {
		t.Errorf("retrieval line %q does not say %q — it is counting search hits rather than what the model saw", line, want)
	}
}

// TestRetrievalLineIsSilentWhenNothingWasSearched. A greeting has no content words, so
// no search runs at all, and the groups come back empty for that reason rather than
// because the spaces are. "(nothing)" about a search that never happened is a claim
// the node cannot support.
func TestRetrievalLineIsSilentWhenNothingWasSearched(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(context.Context, []string, routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "Hello."}, nil
	}

	// An emoji has no letters or digits, so memory.Terms yields nothing and no
	// search is issued. "hi" would not do: it is two letters and lore would be
	// asked for entries containing it.
	if err := rig.unit.Handle(context.Background(), directInbound("🙂")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := rig.tr.sentTextsRaw(); len(got) != 1 || got[0] != "Hello." {
		t.Errorf("sent %q, want the bare reply: nothing was searched, so there is nothing to report", got)
	}
}

// TestRetrievalLineSaysASpaceCouldNotBeRead rather than counting it to nothing, for
// the same reason the prompt says so to the model: an error rendered as "found
// nothing" is a lie the member might act on, and this one they can act on immediately
// by asking again.
func TestRetrievalLineSaysASpaceCouldNotBeRead(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.errFor["household"] = errors.New("lore is down")
	rig.router.fn = func(context.Context, []string, routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "Answered."}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("boiler")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	line, _, _ := strings.Cut(rig.tr.sentTextsRaw()[0], "\n")
	if !strings.Contains(line, "the household memory (couldn't be read)") {
		t.Errorf("retrieval line %q renders a failed search as an empty one", line)
	}
	if strings.Contains(line, "the household memory (nothing)") {
		t.Error("a space that could not be read was reported as holding nothing")
	}
}

// TestRetrievalLineNamesOnlyTheSpacesInScope: the group conversation cannot read a
// private memory, so its line must not mention one. The prompt's disclosure and this
// line are the two places a member is told what was visible, and they have to agree.
func TestRetrievalLineNamesOnlyTheSpacesInScope(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testGroupScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["household"] = []memory.Entry{
		entry("household", "Boiler code", "The code is 4417.", "validated"),
	}
	rig.router.fn = func(context.Context, []string, routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "4417."}, nil
	}

	if err := rig.unit.Handle(context.Background(), groupInbound("boiler")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	line, _, _ := strings.Cut(rig.tr.sentTextsRaw()[0], "\n")
	if want := "<i>🔍 searched the household memory (1 entry)</i>"; line != want {
		t.Errorf("group retrieval line = %q, want %q", line, want)
	}
	if strings.Contains(line, "private") {
		t.Error("the group's retrieval line names a private memory it cannot read")
	}
}

// TestReadNoticesOffSendsTheReplyAlone is the household's opt-out, and the guard that
// it removes the line rather than emptying it.
func TestReadNoticesOffSendsTheReplyAlone(t *testing.T) {
	opts := testOptions()
	opts.ReadNotices = ReadNoticesOff
	rig, err := newTestRig(fixedResolver(testDirectScope()), opts)
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["david-private"] = []memory.Entry{
		entry("david-private", "Boiler service", "Serviced in March.", "validated"),
	}
	rig.router.fn = func(context.Context, []string, routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "It was serviced in March."}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("boiler")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := rig.tr.sentTextsRaw(); len(got) != 1 || got[0] != "It was serviced in March." {
		t.Errorf("sent %q, want the reply on its own", got)
	}
	// The search still happened: this is a setting about what is said, not about
	// what is read.
	if len(rig.mem.searchedSpaces()) == 0 {
		t.Error("turning the notice off stopped the retrieval as well")
	}
}

// TestRetrievalLineStaysOutOfHistory. It is the node reporting on itself, not the
// assistant's words, and a line fed back as an assistant turn teaches the model to
// write those lines itself — at which point the member cannot tell a real accounting
// of what was read from a fabricated one.
func TestRetrievalLineStaysOutOfHistory(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.mem.bySpace["david-private"] = []memory.Entry{
		entry("david-private", "Boiler service", "Serviced in March.", "validated"),
	}
	rig.router.fn = func(context.Context, []string, routing.Request) (routing.Completion, error) {
		return routing.Completion{Text: "It was serviced in March."}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("boiler")); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	second := directInbound("boiler again")
	second.MessageID = 2
	if err := rig.unit.Handle(context.Background(), second); err != nil {
		t.Fatalf("second Handle: %v", err)
	}

	req, ok := rig.router.lastRequest()
	if !ok {
		t.Fatal("no request reached the router")
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "🔍 searched ") {
			t.Fatalf("the retrieval line reached the model as a %s message: %q", m.Role, m.Content)
		}
	}
}

// TestTypingIndicatorCoversTheWaitAndStops.
//
// Every turn of the live run against a real model took fifteen to twenty seconds, with
// nothing on screen for any of it. In a family group chat that is not a slow assistant,
// it is a broken one: people re-send, then stop using it. Telegram's answer is
// sendChatAction, and the whole of the requirement is that it starts before the wait
// and stops when the wait ends.
//
// The router blocks until an indicator has been seen, which makes the first half an
// assertion rather than a race. The second half is assertable at all because the turn
// waits for the indicator's goroutine before returning: once Handle is back, nothing
// can send another action, so a count taken afterwards is final.
func TestTypingIndicatorCoversTheWaitAndStops(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	typed := make(chan struct{})
	rig.tr.typed = typed
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		// The fifteen seconds, in the only form a unit test can hold: the model does
		// not answer until the member has been shown something.
		select {
		case <-typed:
		case <-ctx.Done():
			return routing.Completion{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return routing.Completion{}, errors.New("no typing indicator while the member waited")
		}
		return routing.Completion{Text: "Under the stairs.", Endpoint: "fake", Tier: "local"}, nil
	}

	if err := rig.unit.Handle(context.Background(), directInbound("where is the stopcock?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := rig.tr.typingCount(testMemberChat); got == 0 {
		t.Fatal("the member waited on the model with no typing indicator at all")
	}
	if got := rig.tr.typingCount(testGroupChat); got != 0 {
		t.Errorf("a chat nobody was waiting in got %d typing indicators", got)
	}
	// Every indicator went out before the reply did. The count of messages sent as
	// of the last indicator is zero, so nothing was still typing once there was
	// something to read.
	if got := rig.tr.sendsWhenTypingStopped(); got != 0 {
		t.Errorf("the last typing indicator went out after %d message(s) had been sent; it must stop when the reply lands", got)
	}
	settled := rig.tr.typingCount(testMemberChat)
	time.Sleep(20 * time.Millisecond)
	if got := rig.tr.typingCount(testMemberChat); got != settled {
		t.Errorf("typing count went from %d to %d after the turn returned; the indicator outlived the turn", settled, got)
	}
}

// TestLockedConversationShowsNoTypingIndicator: a locked member gets one notice and
// nothing else. An indicator there would be the node claiming to be working on an
// answer it has already decided not to produce — and it would keep the "…is typing"
// header up over a chat that is going to stay silent.
func TestLockedConversationShowsNoTypingIndicator(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.sessions.Lock("david")

	if err := rig.unit.Handle(context.Background(), directInbound("where is the stopcock?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := rig.tr.typingCount(testMemberChat); got != 0 {
		t.Errorf("a locked conversation showed %d typing indicators; the turn stops at the notice", got)
	}
}

// TestReplyMarkdownReachesTheMemberAsFormatting is D1 at the seam it actually
// crosses.
//
// The reply is the only text on this path kenward did not write: it is converted.
// Everything else in the message — the retrieval line, a question, an entry quoted
// back — was escaped by transport's marks and is not reparsed.
func TestReplyMarkdownReachesTheMemberAsFormatting(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text:     "- **Mains water** — the stopcock\n\nRun `kenward status`:\n```sh\nkenward status\n```",
			Endpoint: "monster", Tier: "local",
		}, nil
	}
	if err := rig.unit.Handle(context.Background(), directInbound("where is the stopcock?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	texts := rig.tr.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("sent %d messages, want 1: %v", len(texts), texts)
	}
	got := texts[0]
	for _, want := range []string{
		"<b>Mains water</b>",
		"<code>kenward status</code>",
		"<pre>kenward status</pre>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the reply reached the member without %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "```") {
		t.Errorf("the member was shown Markdown they never asked for:\n%s", got)
	}
}
