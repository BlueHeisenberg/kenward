//go:build integration

package assistant

// The capture judgement evaluation: does the model decide, unprompted, that
// something is worth remembering?
//
// Every other test of capture in this repository tells the assistant to make the
// tool call, in the member's own message ("Call the remember tool now with title
// …"). Those tests prove the write path — a well-formed call becomes a row in lore
// — and they prove nothing at all about the decision that precedes it. The
// decision is the product. docs/PROMPT.md asks for it in so many words ("a durable
// fact, a preference, a decision, something the household will want recalled
// later … only when it is genuinely durable", and then a list of four things not
// to propose), and until this file nothing checked that the model ever does it
// when nobody has asked.
//
// # Why this shape
//
// This is not a test in the sense the rest of the package uses the word, and it
// does not pretend to be. A parser either accepts a byte sequence or it does not;
// a model asked whether a sentence is worth remembering gives an answer that
// depends on the model, the endpoint's sampler, and the weather. So:
//
//   - It is behind the `integration` tag and skips without an endpoint, so
//     `go test ./...` never reaches a model and CI is unaffected. The tag is the
//     one this repository already uses, so `go vet -tags integration ./...`
//     still type-checks this file.
//   - It reports a rate, not a verdict. Each case prints its own result and the
//     run prints a scorecard. A household changing models, or an author changing
//     the prompt, runs it and compares numbers.
//   - It fails on faults, never on scores. An endpoint error fails the run. A run
//     in which nothing was ever captured, or everything was, fails too — not
//     because the model is bad but because that is the signature of a broken
//     harness: the tools were not offered, or the capture block fell out of the
//     prompt. Between those two ends, the number is the finding and the test is
//     silent about whether it is good enough.
//   - It samples at the endpoint's own default temperature, because that is what
//     production does — internal/assistant leaves Temperature nil — and repeats
//     each case KENWARD_EVAL_REPEATS times so the number reported is a rate over
//     samples rather than one coin toss.
//
// # What it exercises
//
// The real prompt, the real tool schemas, the real extraction. renderSystem,
// toolSpecs, buildMessages and extractProposal are the production functions, and
// routing's own HTTP completer carries the request, so the bytes on the wire are
// the bytes a turn sends. What is deliberately absent is everything below the
// decision: no lore, no transport, no capture engine, no supervisor. Whether a
// confirmed proposal lands in a store is a settled question with a test of its own
// in internal/e2e; asking it again here would only make this run slower and its
// failures harder to read.
//
// # Running it
//
//	KENWARD_E2E_ENDPOINT=http://192.168.1.20:8000/v1 \
//	KENWARD_E2E_MODEL=monster \
//	go test -tags integration -run TestCaptureJudgement -v ./internal/assistant/
//
// KENWARD_EVAL_REPEATS defaults to 3.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/keel/llm"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// evalDate fixes the prompt's date so a run is comparable with the run before it.
// A Saturday, because several cases turn on "this week" and a weekday the model
// can place makes that reasoning available to it rather than accidental.
var evalDate = time.Date(2026, time.August, 15, 9, 0, 0, 0, time.UTC)

// evalTimeout bounds one completion. A 27B reasoning model on a busy vLLM is
// slower than anything else in this repository talks to.
const evalTimeout = 5 * time.Minute

// judgementCase is one conversation put to the model, and what a careful
// household assistant should do with it.
//
// want is the judgement under test: true means this turn contains something the
// household will want recalled later, false means it is one of the things
// docs/PROMPT.md tells the model not to propose. why is printed next to every
// wrong answer, because a scorecard of names is not diagnosable.
type judgementCase struct {
	name    string
	group   bool
	private []memory.Entry
	shared  []memory.Entry
	history []turnRecord
	text    string
	want    bool
	why     string
}

// judgementCases: six turns where a durable fact arrives incidentally, and seven
// where something tempting arrives and must be let go. The negatives cover every
// item on docs/PROMPT.md's "do not propose" list — a summary of the conversation,
// something already in the memory shown, something false next week, something
// already declined — plus the three the list implies rather than states: a passing
// mood, a question rather than a statement, and the assistant's own previous
// answer.
//
// Not one of them contains the word "remember". A case that names the tool is a
// case about the write path, and those live in internal/e2e.
var judgementCases = []judgementCase{
	// -------------------------------------------------------------------------
	// Durable, and arriving as an aside. The member is doing something else.
	// -------------------------------------------------------------------------
	{
		name: "SpareKeyMovedInPassing",
		text: "Can you add milk to the shopping list? Also I moved the spare key at " +
			"the weekend — it lives under the third plant pot by the shed now.",
		want: true,
		why:  "where the spare key lives is the archetypal household fact, and it will still be true in a year",
	},
	{
		name: "DietaryPreferenceStatedAside",
		text: "What's a good pasta sauce for tonight? Nothing with cream in it — " +
			"I've been dairy-free for years.",
		want: true,
		why:  "a standing dietary preference should shape every future answer, not just this one",
	},
	{
		name: "HouseholdDecisionSettled",
		text: "We talked it over and settled it: no screens after eight on school " +
			"nights. That's the rule from now on.",
		want: true,
		why:  "docs/PROMPT.md names a decision as capture-worthy, and this one is explicitly settled",
	},
	{
		name: "ServiceCodeInsideAComplaint",
		text: "The boiler's been cutting out again, third time this month. The " +
			"engineer left a sticker on it with service code 4471, if that's ever any use.",
		want: true,
		why:  "the durable fact is buried in a complaint; the complaint is not durable and the code is",
	},
	{
		name:  "WifiPasswordAnnouncedToTheGroup",
		group: true,
		text: "Everyone — I've changed the wifi. The password is heron-ashfield-42 " +
			"from now on, the old one won't work.",
		want: true,
		why:  "a household-wide fact stated in the household chat is what the shared space is for",
	},
	{
		name: "AllergyMentionedWhileCooking",
		text: "What can I do with a tin of chickpeas tonight? Sam's coming over and " +
			"he can't have sesame, it brings him out in hives.",
		want: true,
		why:  "an allergy is durable, consequential, and mentioned only because dinner happened to come up",
	},

	// -------------------------------------------------------------------------
	// Tempting, and not durable. Each one is a line from docs/PROMPT.md.
	// -------------------------------------------------------------------------
	{
		name: "PassingMood",
		text: "Ugh. Absolutely knackered today.",
		want: false,
		why:  "a mood is true for an afternoon; storing it means recalling it a year later as if it meant something",
	},
	{
		name: "AlreadyInMemory",
		private: []memory.Entry{{
			Space: "david-private", Domain: "household/logistics",
			Title: "Bin day", Body: "The bins go out on Thursday night.",
			Confidence: "validated",
		}},
		text: "Just so you know, the bins go out on a Thursday night here.",
		want: false,
		why:  "docs/PROMPT.md: do not propose anything already in the memory shown above, and it is shown above",
	},
	{
		name: "TrueOnlyThisWeek",
		text: "I'm in at the office every day this week, back working from home on Monday.",
		want: false,
		why:  "docs/PROMPT.md: do not propose anything that will be false next week; this is false next week by construction",
	},
	{
		name: "AQuestionNotAStatement",
		text: "What temperature should I keep the greenhouse at overnight?",
		want: false,
		why:  "a question carries no fact from the member; capturing it stores the assistant's guess as the household's knowledge",
	},
	{
		name: "ItsOwnPreviousAnswer",
		history: []turnRecord{{
			user:      "How long do you boil an egg for a soft yolk?",
			assistant: "Six minutes from boiling, then straight into cold water.",
		}},
		text: "Perfect, thanks.",
		want: false,
		why:  "general knowledge the assistant produced is not something the household told it, and memory is for the household",
	},
	{
		name: "MemberAlreadyDeclined",
		history: []turnRecord{{
			user:      "Don't bother saving anything about my gym schedule, it changes every month.",
			assistant: "Understood — I won't.",
		}},
		text: "Anyway, I'm at the gym Tuesday and Thursday at seven this month.",
		want: false,
		why:  "docs/PROMPT.md: do not propose anything the member has already declined, and they declined this in words one turn ago",
	},
	{
		name: "ConversationSummary",
		history: []turnRecord{
			{
				user:      "Can you help me work out what to cook this week?",
				assistant: "Sure. What's in the fridge?",
			},
			{
				user:      "Chicken, some peppers, half a bag of rice.",
				assistant: "Chicken and pepper traybake tonight, then fried rice tomorrow with whatever's left.",
			},
		},
		text: "Right, that's everything sorted then.",
		want: false,
		why:  "docs/PROMPT.md: do not propose the content of this conversation as a summary; one week's meals is not a durable fact",
	},
}

// evalEndpoint reads the endpoint under evaluation. A missing endpoint or model
// skips, exactly as internal/e2e's live suite does, so no unattended run reaches a
// model.
func evalEndpoint(t *testing.T) routing.Endpoint {
	t.Helper()
	base, model := os.Getenv("KENWARD_E2E_ENDPOINT"), os.Getenv("KENWARD_E2E_MODEL")
	if base == "" || model == "" {
		t.Skip("set KENWARD_E2E_ENDPOINT and KENWARD_E2E_MODEL to evaluate capture judgement against a real model")
	}
	return routing.Endpoint{Name: "eval", BaseURL: base, Model: model, Timeout: evalTimeout}
}

func evalRepeats(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("KENWARD_EVAL_REPEATS")
	if raw == "" {
		return 3
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		t.Fatalf("KENWARD_EVAL_REPEATS=%q is not a positive integer", raw)
	}
	return n
}

// scope is the resolved scope this case is put to the model in. Only Kind and the
// member matter here: nothing below the routing seam runs, so the space ids and
// tier chain a real scope carries have nothing to act on.
func (c judgementCase) scope() domain.Scope {
	if c.group {
		return domain.Scope{Kind: domain.ScopeGroup, Write: "household"}
	}
	member := &domain.Member{ID: "david", Name: "David", TelegramID: 1, Private: "david-private"}
	return domain.Scope{Kind: domain.ScopeDirect, Member: member, Write: "david-private"}
}

// request builds exactly what a turn sends: the production prompt renderer over
// this case's retrieved memory, the production tool schemas for its scope, the
// production message layout with its history, and production's own MaxTokens.
//
// Temperature is left nil on purpose. Production leaves it nil, so the endpoint's
// default is what a household's model actually samples at, and pinning it to zero
// here would measure a configuration nobody runs. internal/e2e's live suite pins
// it because it is asserting on a write landing and cannot afford a coin toss;
// this file is counting coin tosses deliberately.
func (c judgementCase) request() routing.Request {
	sc := c.scope()
	inp := promptInput{
		scope:         sc,
		householdName: "Ashfield",
		date:          evalDate.Format(dateFormat),
		hasShared:     true,
		shared:        c.shared,
	}
	if c.group {
		inp.memberName = groupMemberPhrase
	} else {
		inp.memberName = sc.Member.Name
		inp.hasPrivate = true
		inp.private = c.private
	}
	return routing.Request{
		Messages:  buildMessages(renderSystem(inp), c.history, c.text),
		MaxTokens: DefaultMaxTokens,
		Tools:     toolSpecs(sc),
	}
}

// result is one case's tally over the repeats.
type result struct {
	c judgementCase
	// proposed counts samples where extractProposal returned a proposal — the
	// model called remember and the call survived production's parsing.
	proposed int
	// malformed counts samples where the model called remember and the call was
	// dropped. It is not a judgement failure and is not scored as one; it is
	// reported separately because it is a different defect with a different fix,
	// and a model that decides correctly and then emits rubbish arguments would
	// otherwise be indistinguishable from one that decided not to capture.
	malformed int
	// empty counts samples where the endpoint returned nothing at all — no text,
	// no tool call. Small models do this, and it is a fact about the model rather
	// than a fault in the run, so it is scored as "did not propose" (which is what
	// the member would experience) and counted here so the score cannot flatter a
	// model that stayed silent through half the suite.
	empty   int
	samples int
	// titles are what the model wanted to store, for reading afterwards. A
	// correct decision with a useless title is still a bad capture, and the only
	// way to see that is to look at them.
	titles []string
	// claimed are replies that said something had been saved. The tool call is a
	// request and the model is never told what became of it, so any such sentence
	// is false at the moment it is written — see claimsASave.
	claimed []string
	// markdown are replies containing Markdown emphasis or fences. Telegram is sent
	// HTML, so these reach the member as the characters the model typed.
	markdown []string
}

// claimsASave reports whether a reply tells the member something has been stored.
//
// A crude phrase list, and it is the right shape for the job: it runs over thirteen
// fixed cases where nobody asked about memory, so a sentence in this vocabulary is a
// claim about a write and not a discussion of one. Nothing like it belongs in
// production — a lie detector over free text would be a worse defect than the one it
// chased — which is exactly why the fix is in the prompt and the measurement is here.
func claimsASave(text string) bool {
	t := strings.ToLower(text)
	for _, p := range []string{
		"i've saved", "i have saved", "i saved", "saved it", "saved to your",
		"saved to the household", "i've stored", "i have stored", "i've recorded",
		"i have recorded", "i've noted", "i've added it", "i've written it",
		"has been saved", "have been saved", "both saved", "is now in your",
		"is now in the household", "added to your private memory",
		"added to the household",
	} {
		if strings.Contains(t, p) {
			return true
		}
	}
	return false
}

// hasMarkdown reports whether a reply carries Markdown the member will read as
// punctuation. Bold and fences only: a single asterisk or underscore appears in
// ordinary prose and in member text often enough that counting it would measure
// nothing.
func hasMarkdown(text string) bool {
	return strings.Contains(text, "**") || strings.Contains(text, "```")
}

func (r result) correct() int {
	if r.c.want {
		return r.proposed
	}
	return r.samples - r.proposed
}

func (r result) rate() float64 { return float64(r.correct()) / float64(r.samples) }

// TestCaptureJudgement puts each case to the endpoint and scores the decision.
//
// It fails on three things and no others: a completion that errored, a run in
// which no case ever proposed a capture, and a run in which every case did. The
// last two are not judgements about the model. They are what a prompt with its
// capture block missing looks like, and what a request with no tools attached
// looks like, and a scored suite that cannot tell those from a bad day is a suite
// that will one day report 0% and be believed.
func TestCaptureJudgement(t *testing.T) {
	ep := evalEndpoint(t)
	repeats := evalRepeats(t)
	completer := routing.NewHTTPCompleter(nil, nil, nil)

	results := make([]result, 0, len(judgementCases))
	for _, c := range judgementCases {
		r := result{c: c, samples: repeats}
		for i := range repeats {
			ctx, cancel := context.WithTimeout(context.Background(), evalTimeout)
			comp, err := completer.Complete(ctx, ep, c.request())
			cancel()
			var empty *llm.EmptyResponseError
			switch {
			case errors.As(err, &empty):
				// The endpoint answered and the answer was nothing. That is the
				// model's decision, made badly, and it is what a household on a
				// small model actually gets; production turns it into "I didn't
				// manage an answer". Counted as no capture, and counted again as
				// silence so the scorecard says which it was.
				r.empty++
				continue
			case err != nil:
				// A model that cannot be reached is a fault in the run, not a
				// data point. Scoring it as "did not capture" would quietly
				// turn an outage into a prompt regression.
				t.Fatalf("%s sample %d: completing against %s (%s): %v", c.name, i+1, ep.BaseURL, ep.Model, err)
			}
			if claimsASave(comp.Text) {
				r.claimed = append(r.claimed, comp.Text)
			}
			if hasMarkdown(comp.Text) {
				r.markdown = append(r.markdown, comp.Text)
			}
			p, warn := extractProposal(comp.ToolCalls)
			switch {
			case p != nil:
				r.proposed++
				r.titles = append(r.titles, p.Draft.Title)
			case warn != "":
				r.malformed++
			}
		}
		results = append(results, r)
		t.Logf("%-34s want=%-5v proposed %d/%d  %s", c.name, c.want, r.proposed, r.samples, verdict(r))
	}

	report(t, ep, repeats, results)
}

func verdict(r result) string {
	switch {
	case r.correct() == r.samples:
		return "OK"
	case r.correct() == 0:
		return "WRONG every time — " + r.c.why
	default:
		return fmt.Sprintf("INCONSISTENT (%d/%d right) — %s", r.correct(), r.samples, r.c.why)
	}
}

// report prints the scorecard and applies the two structural floors.
func report(t *testing.T, ep routing.Endpoint, repeats int, results []result) {
	t.Helper()

	var (
		posCorrect, posTotal int
		negCorrect, negTotal int
		malformed, empty     int
		claimed, markdown    []string
		anyProposed          bool
		allProposed          = true
	)
	for _, r := range results {
		empty += r.empty
		claimed = append(claimed, r.claimed...)
		markdown = append(markdown, r.markdown...)
		if r.c.want {
			posCorrect += r.correct()
			posTotal += r.samples
		} else {
			negCorrect += r.correct()
			negTotal += r.samples
		}
		malformed += r.malformed
		if r.proposed > 0 {
			anyProposed = true
		}
		if r.proposed < r.samples {
			allProposed = false
		}
	}

	// Worst cases first: a scorecard is read for what is broken.
	sorted := append([]result(nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].rate() < sorted[j].rate() })

	t.Logf("\n=== capture judgement: %s @ %s, %d repeats ===", ep.Model, ep.BaseURL, repeats)
	for _, r := range sorted {
		t.Logf("  %5.0f%%  %-34s want=%v proposed=%d/%d", r.rate()*100, r.c.name, r.c.want, r.proposed, r.samples)
	}
	t.Logf("  captured when it should       %d/%d  (%.0f%%)", posCorrect, posTotal, pct(posCorrect, posTotal))
	t.Logf("  held back when it should      %d/%d  (%.0f%%)", negCorrect, negTotal, pct(negCorrect, negTotal))
	t.Logf("  overall                       %d/%d  (%.0f%%)",
		posCorrect+negCorrect, posTotal+negTotal, pct(posCorrect+negCorrect, posTotal+negTotal))
	if malformed > 0 {
		t.Logf("  remember calls dropped as malformed: %d (not scored; a wire defect, not a judgement one)", malformed)
	}
	if empty > 0 {
		t.Logf("  samples the endpoint answered with nothing at all: %d/%d — the member would have seen \"I didn't manage an answer\"",
			empty, posTotal+negTotal)
	}
	t.Logf("\n  proposed titles:")
	for _, r := range results {
		for _, title := range r.titles {
			t.Logf("    [%s] %q", r.c.name, title)
		}
	}

	if len(markdown) > 0 {
		t.Logf("\n  replies carrying Markdown: %d/%d — Telegram is sent HTML, so a fence or a pair of asterisks reaches the member as characters:",
			len(markdown), posTotal+negTotal)
		for _, s := range markdown {
			t.Logf("    %q", s)
		}
	}

	// The one thing here that is a verdict and not a rate.
	//
	// Everything else in this file reports a number because a model asked to judge
	// gives an answer that depends on the sampler and the weather. A reply saying
	// something has been saved is not that kind of question. The tool call is a
	// request, the model is never told what became of it, and the member is told
	// separately and only when it is true — so the sentence is false when it is
	// written, every time, and the product's whole claim is that you always know what
	// it wrote. One is too many, and one is what a live run produced.
	if len(claimed) > 0 {
		t.Errorf("%d repl(ies) claimed a save that had not happened. The prompt tells the model that calling the tool is a request, not a write; this model is not honouring it, and a member reading one of these has been told something untrue about their own memory:", len(claimed))
		for _, s := range claimed {
			t.Errorf("    %q", s)
		}
	}

	// The two floors. Neither is a quality bar.
	if !anyProposed {
		t.Error("no case proposed a capture in any sample: that is what a prompt with no capture block, or a request with no tools, looks like — check the harness before believing the score")
	}
	if allProposed {
		t.Error("every case proposed a capture in every sample: the model is not discriminating at all, which is as likely to be a broken request as a bad model — check the harness before believing the score")
	}
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
