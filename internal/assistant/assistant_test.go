package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/llm"

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
	"github.com/BlueHeisenberg/kenward/internal/scope"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

func TestNewRejectsMissingDeps(t *testing.T) {
	base := func() Deps {
		mem := newFakeMemory()
		tr := &fakeTransport{}
		return Deps{
			Resolve:   fixedResolver(testDirectScope()),
			Memory:    mem,
			Router:    &fakeRouter{},
			Transport: tr,
			Sessions:  newFakeSessions(),
			Capture:   capture.New(mem, tr, capture.Options{}),
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

	if err := rig.unit.Handle(context.Background(), directInbound("when do the bins go out?")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Both spaces searched, one query each, in scope order.
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
		"## Excerpts from David's private memory",
		"- Coffee order [validated]",
		"## Excerpts from the household's shared memory",
		"- Bin day [hardened] (UPDATED)",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if got := rig.router.chains[0]; len(got) != 1 || got[0] != "local" {
		t.Errorf("router chain %v, want [local]", got)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "remember" {
		t.Errorf("request tools %+v, want exactly the remember tool", req.Tools)
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
	// Nothing was shown, so nothing is labelled as an excerpt and the excerpt note
	// has nothing to describe.
	if strings.Contains(sys, "Excerpts from") {
		t.Error("an empty section is headed as excerpts")
	}
	if strings.Contains(sys, "search excerpts") {
		t.Error("excerpt note rendered with no excerpts shown")
	}
}

func TestMalformedRememberIsDroppedHarmlessly(t *testing.T) {
	rig, err := newTestRig(fixedResolver(testDirectScope()), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	rig.router.fn = func(ctx context.Context, chain []string, req routing.Request) (routing.Completion, error) {
		return routing.Completion{
			Text: "Noted.",
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
	if len(texts) != 1 || texts[0] != "Noted." {
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
		return routing.Completion{Text: "done"}, nil
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
			if txt == queuedText {
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
		if txt == droppedText {
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

// TestPutFailureUncertaintyReachesMember tests the join the packages' own tests
// miss: the member taps Save, the store fails, and the member — not just the log —
// hears that the write is uncertain.
func TestPutFailureUncertaintyReachesMember(t *testing.T) {
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
	var uncertain bool
	for _, txt := range rig.tr.sentTexts() {
		if strings.Contains(txt, "can't confirm") && strings.Contains(txt, "Check before saving it again") {
			uncertain = true
		}
		if strings.Contains(txt, "Saved") {
			t.Errorf("member saw a confirmation for a write that failed: %q", txt)
		}
	}
	if !uncertain {
		t.Fatalf("member never told the write is uncertain; sent: %v", rig.tr.sentTexts())
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
		if txt == queuedText || txt == droppedText {
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
	deps := Deps{
		Resolve:   fixedResolver(testDirectScope()),
		Memory:    mem,
		Router:    &fakeRouter{},
		Transport: tr,
		Sessions:  newFakeSessions(),
		Capture:   capture.New(mem, tr, capture.Options{}),
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
