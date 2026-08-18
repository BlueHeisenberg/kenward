//go:build integration

package assistant

// The capture judgement evaluation: does the model decide, unprompted, that
// something is worth remembering?
//
// Every other test of capture in this repository asks for the write in the member's
// own message ("Remember this just for me: …"). Those tests prove the write path — a
// well-formed call becomes a row in lore — and they prove nothing at all about the
// decision that precedes it, because the member made it. The
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
//   - It is behind the `integration` tag, so `go test ./...` never reaches a model
//     and CI is unaffected — the tag does that on its own, and no CI workflow here
//     passes it. The tag is the one this repository already uses, so `go vet -tags
//     integration ./...` still type-checks this file. Without an endpoint it fails
//     rather than skipping; see evalEndpoint for why that is the safer default.
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
// KENWARD_EVAL_REPEATS defaults to 3. Without an endpoint this fails rather than
// skipping; KENWARD_E2E_SKIP=1 waives it. See evalEndpoint.
//
// It wants the endpoint to itself. One sample is a 27B reasoning turn, and a run
// sharing the GPU with anything else has been seen to blow evalTimeout and fail the
// whole suite on a completion that would otherwise have arrived. Since that is not
// always arrangeable, evalComplete retries a transport error once — which is the
// difference between measuring the prompt and measuring the machine's diary — and
// still fails the run when the second attempt misses too.
//
// TestRequestedCapture is the second population, scored separately: the same model, the
// same prompt, on turns where the member asks for the write in plain words. See
// requestedCases for why the two are never added together.

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

	"github.com/BlueHeisenberg/kenward/internal/capture"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
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
// case about the write path, and those live in internal/e2e — and, for what the model
// decides to do and say about one, in requestedCases below. The two populations are
// scored separately and never merged: this one measures an unprompted judgement, and a
// case where the member has already made the decision would raise its rate while
// measuring something else entirely.
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

// requestedCases: four turns where the member asks for the write in so many words.
//
// This is the second population, and it exists because the first cannot see the defect
// that matters most. claimsASave runs over every reply in this file, but until now
// every reply it saw came from a turn where nobody had mentioned memory — the one
// population where a false claim is least likely, because the model has no reason to
// talk about storage at all. The risk lives here: a member who says "remember this"
// is owed an answer, is expecting some acknowledgement, and is the member the live
// run's false claim was made to.
//
// want is true for all four. The member decided; the model's only job is to make the
// call. That makes the rate here a different measurement from judgementCases' — not a
// judgement but a compliance — which is why it is reported under its own heading and
// never added to the other. Merging them would move the headline number for a reason
// that has nothing to do with judgement.
//
// NamesTheToolOutright is a regression case with a date on it. After the reply-
// truthfulness paragraph was added to captureText, this exact shape produced no tool
// call at all, greedily and repeatably: the model worked the arguments out in its
// reasoning trace and then declined to emit them, having read a dense block of
// "do not mention the tool" as a reason not to use it. If this case ever reads 0/N
// again, that is what has happened.
var requestedCases = []judgementCase{
	{
		name: "NamesTheToolOutright",
		text: `Call the remember tool now with title "Boiler service code" and body ` +
			`"The boiler service code is 4471."`,
		want: true,
		why:  "the member named the tool and dictated its arguments; there is no judgement left to make",
	},
	{
		name: "PlainRequestPrivate",
		text: "Remember this just for me: the boiler service code is 4471.",
		want: true,
		why:  "an explicit request in a direct scope is the commonest way a member ever asks for a write",
	},
	{
		name:  "PlainRequestToTheGroup",
		group: true,
		text:  "Remember for everyone: the wifi password is heron-ashfield-42.",
		want:  true,
		why:   "the group scope has one destination and the member has asked for it by name",
	},
	{
		name: "RequestInsideAnOrdinaryQuestion",
		text: "What's the best way to bleed a radiator? Also can you remember that " +
			"the stopcock is under the stairs, I keep forgetting.",
		want: true,
		why:  "the request arrives alongside a question the model must also answer, which is where narration creeps in",
	},
}

// TestRequestedCapture scores the member-requested path: does the call happen, and
// does the reply stay quiet about it?
//
// Both halves are here on purpose. They are the two ways the same paragraph can fail,
// they pull against each other, and measuring one without the other is how a prompt
// change gets called a fix. Wording that frightens the model away from claiming a save
// can frighten it away from the call; wording that frees the call can free the
// narration with it. A run reports both — the capture rate, and every reply that
// narrated one — so the trade is visible in one scorecard rather than discovered later.
//
// The narration count used to be an error and is now a number. It was an error while a
// claimed save with no call reached the member unchallenged; the product corrects those
// now, and what is left is a prompt-compliance rate on turns that did produce a card.
// See the report at the bottom.
//
// It fails on a completion error and on the one structural floor that means the
// suppression is back: not a single requested case produced a call in any sample. A
// member who says "remember this" and is answered by a model that called nothing is
// looking at the defect this population was added to catch, and no rate reporting is
// going to make that ambiguous enough to be worth softening.
func TestRequestedCapture(t *testing.T) {
	ep := evalEndpoint(t)
	repeats := evalRepeats(t)
	var (
		results     []result
		claimed     []string
		anyProposed bool
	)
	for _, c := range requestedCases {
		r := result{c: c, samples: repeats}
		for i := range repeats {
			comp, err := evalComplete(t, ep, c.request(), c.name, i+1)
			var empty *llm.EmptyResponseError
			switch {
			case errors.As(err, &empty):
				r.empty++
				continue
			case err != nil:
				t.Fatalf("%s sample %d: completing against %s (%s): %v", c.name, i+1, ep.BaseURL, ep.Model, err)
			}
			if claimsASave(comp.Text) {
				r.claimed = append(r.claimed, comp.Text)
			}
			p, warn := extractProposal(comp.ToolCalls)
			switch {
			case p != nil:
				r.proposed++
				r.titles = append(r.titles, p.Draft.Title)
				if r.targets == nil {
					r.targets = map[capture.Target]int{}
				}
				r.targets[p.Target]++
			case warn != "":
				r.malformed++
			}
			// The dangerous half, counted on exactly the turns where it is dangerous:
			// the member asked for a write, no call arrived, and the reply would leave
			// them believing one happened — bare, or naming the save outright. Whether
			// the member is told is production's own falseSave and not a copy of its
			// reasoning, because a scorecard of a copy scores the copy; the split into
			// two lists is only for reading, since the two shapes fail differently.
			// These cases render an English prompt with the default persona, so English
			// is the catalogue production would use.
			if p == nil {
				switch {
				case !falseSave(lang.For(""), c.text, comp.Text):
					r.uncaught = append(r.uncaught, comp.Text)
				case lang.For("").IsBareAcknowledgement(comp.Text):
					r.bare = append(r.bare, comp.Text)
				default:
					r.annotated = append(r.annotated, comp.Text)
				}
			}
			// Kept whatever happened: a reply is the only evidence of a call that
			// was reasoned about and then not made, and that is the failure mode.
			r.replies = append(r.replies, comp.Text)
		}
		if r.proposed > 0 {
			anyProposed = true
		}
		claimed = append(claimed, r.claimed...)
		results = append(results, r)
		t.Logf("%-34s asked for a write, called remember %d/%d  target %s  %s", c.name, r.proposed, r.samples, r.targetTally(), verdict(r))
	}

	var called, total, malformed, empty int
	var bare, annotated, uncaught []string
	targets := map[capture.Target]int{}
	t.Logf("\n=== requested capture: %s @ %s, %d repeats ===", ep.Model, ep.BaseURL, repeats)
	for _, r := range results {
		called += r.proposed
		total += r.samples
		malformed += r.malformed
		empty += r.empty
		bare = append(bare, r.bare...)
		annotated = append(annotated, r.annotated...)
		uncaught = append(uncaught, r.uncaught...)
		for tgt, n := range r.targets {
			targets[tgt] += n
		}
		t.Logf("  %5.0f%%  %-34s called=%d/%d", r.rate()*100, r.c.name, r.proposed, r.samples)
	}
	t.Logf("  called when asked             %d/%d  (%.0f%%)", called, total, pct(called, total))
	// The destination the call named, over the calls that survived. See result.targets:
	// a proposal that says nothing lands on unsure, and unsure is what switches the
	// announce-with-Undo path off in a direct scope. A run with a high call rate and
	// everything on unsure is a run where D-038 never happens.
	t.Logf("  of those, target personal=%d shared=%d unsure=%d",
		targets[capture.TargetPersonal], targets[capture.TargetShared], targets[capture.TargetUnsure])
	t.Logf("  replies claiming a save       %d/%d", len(claimed), total)
	// The second number, and the one the node can actually move: of the turns where
	// the member asked and no call arrived, how many end with the member knowing
	// nothing was stored. Two guards get there by different routes and neither takes
	// the reply away: a bare acknowledgement is one the member asked for, and a reply
	// that names the save carries the fact the member wanted, so both go out whole with
	// the notice underneath.
	//
	// The remainder is the number that matters and the only one nobody can automate:
	// a non-calling turn that neither guard caught is a reply somebody has to read.
	// If it made no claim, the member was never misled and the turn is fine. If it
	// made one in words lang.Catalogue.SaveClaims does not hold, the release blocker
	// is back in a new phrasing and the table is short an entry.
	notCalled := total - called
	told := len(bare) + len(annotated)
	t.Logf("  member asked and got no call  %d/%d", notCalled, total)
	t.Logf("    of those, told nothing was recorded  %d/%d", told, notCalled)
	for _, s := range bare {
		t.Logf("      bare, member asked: %q  +  %q", oneLine(s), lang.For("").NothingSaved)
	}
	for _, s := range annotated {
		t.Logf("      claimed a save: %q  +  %q", oneLine(s), lang.For("").NothingSaved)
	}
	if len(uncaught) > 0 {
		t.Logf("    %d not caught by either guard. Read each one: if it claims a save in words the catalogue does not hold, the blocker is back under a new spelling and SaveClaims is short an entry.", len(uncaught))
		for _, s := range uncaught {
			t.Logf("      uncaught: %q", oneLine(s))
		}
	}
	if malformed > 0 {
		t.Logf("  remember calls dropped as malformed: %d", malformed)
	}
	if empty > 0 {
		t.Logf("  samples the endpoint answered with nothing at all: %d/%d", empty, total)
	}
	for _, r := range results {
		for _, title := range r.titles {
			t.Logf("    [%s] %q", r.c.name, title)
		}
	}
	// Printed always, not only on failure. A member-requested turn that made no call
	// still produced a sentence, and reading that sentence is the whole diagnosis:
	// "Done." with nothing behind it looks identical to a correct reply in a rate.
	t.Logf("\n  replies:")
	for _, r := range results {
		for _, s := range r.replies {
			t.Logf("    [%s] %q", r.c.name, oneLine(s))
		}
	}

	// Reported, and no longer an error. It was one while nothing downstream caught it:
	// a claimed save with no call reached the member as a lie, and the scorecard was
	// the only place it could be seen. The product catches it now — every reply in this
	// list that came off a non-calling turn is in the "told nothing was recorded" count
	// above — so what is left here is prompt compliance. docs/PROMPT.md asks the model
	// not to narrate a capture at all, and it still does on some turns that produce a
	// card: harmless to the member, who gets the card, and worth watching, because the
	// wording that frees the call is the wording that frees the narration.
	//
	// It is also no longer comparable with the number a run printed before this change.
	// claimsASave used to be a short private list that matched none of "Saved …",
	// "Noted …" or "Got it …"; it is lang.Catalogue.SaveClaims now, and it sees them,
	// so a run that reported 0/20 on the old list reports 3/20 on this one for the same
	// replies. Reading the two as a regression would be reading the detector.
	//
	// This restores the rule the rest of the file keeps: it fails on faults — an
	// endpoint that errored, a run where nothing was ever proposed — and never on a
	// score. A red suite on every run the model narrates is a suite nobody reads.
	if len(claimed) > 0 {
		t.Logf("  replies narrating a capture (docs/PROMPT.md asks for none):")
		for _, s := range claimed {
			t.Logf("    %q", oneLine(s))
		}
	}
	if !anyProposed {
		t.Error("not one requested case produced a remember call in any sample: the member asked for a write in plain words and the model made none. That is the capture-suppression regression, not a judgement — read the replies above for the reasoning that talked it out of the call")
	}
}

// evalEndpoint reads the endpoint under evaluation, and **fails rather than skips**
// when there is not one — exactly as internal/e2e's live suite does now, and for the
// same reason.
//
// `go test` prints nothing at all for a package whose tests skip: no reason, no count,
// just `ok`. So a scored evaluation that skips on a missing endpoint is indis-
// tinguishable from one that ran and passed, which is worse here than anywhere else in
// the repository — the whole output of this file is a number somebody is going to
// compare against last week's. Nothing automated pays for it: the `integration` tag
// already keeps this file out of `go test ./...`, and no CI workflow passes the tag.
// KENWARD_E2E_SKIP waives it for somebody running the tagged suite for other reasons.
func evalEndpoint(t *testing.T) routing.Endpoint {
	t.Helper()
	base, model := os.Getenv("KENWARD_E2E_ENDPOINT"), os.Getenv("KENWARD_E2E_MODEL")
	if base != "" && model != "" {
		return routing.Endpoint{Name: "eval", BaseURL: base, Model: model, Timeout: evalTimeout}
	}
	if v := os.Getenv("KENWARD_E2E_SKIP"); v != "" {
		t.Skipf("KENWARD_E2E_SKIP=%s: capture judgement was waived. No model was evaluated.", v)
	}
	t.Fatalf("capture judgement has no endpoint: set KENWARD_E2E_ENDPOINT (e.g. http://192.168.1.20:8000/v1) and KENWARD_E2E_MODEL (e.g. monster).\n" +
		"This is a failure and not a skip on purpose — `go test` prints nothing for a package that skips, so skipping here reports `ok` having scored nothing. " +
		"Set KENWARD_E2E_SKIP=1 to waive it deliberately.")
	return routing.Endpoint{}
}

// evalComplete is one sample, with one retry on a transport error.
//
// The endpoint is a single box with two consumer GPUs, and the file's own advice is
// that a run wants it to itself. That advice is not always followable: a scored run
// takes ten minutes, other work shares the machine, and a 27B reasoning turn that
// queues behind something else blows the header timeout and takes the whole suite with
// it. Losing a run of thirty-nine samples to one queued request measures the GPU's
// diary rather than the prompt.
//
// The retry is deliberately narrow and deliberately not a loop. One transient miss is
// contention; two in a row is the endpoint being down, and that is still a fault in the
// run rather than a data point — scoring it as "did not capture" would turn an outage
// into a prompt regression, which is the thing the caller's t.Fatalf exists to prevent.
// The retry is logged so a run that needed several is legible as a run made on a busy
// machine.
// One completer for the whole run, not one per sample: a fresh client per sample
// discards its connection pool every time, which on a busy endpoint is a new TCP and a
// new queue position for every turn — some of the contention this retry exists to
// absorb.
var evalCompleter = routing.NewHTTPCompleter(nil, nil, nil)

func evalComplete(t *testing.T, ep routing.Endpoint, req routing.Request, name string, sample int) (routing.Completion, error) {
	t.Helper()
	completer := evalCompleter
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), evalTimeout)
		comp, err := completer.Complete(ctx, ep, req)
		cancel()
		var empty *llm.EmptyResponseError
		if err == nil || errors.As(err, &empty) || attempt == 2 {
			return comp, err
		}
		t.Logf("%s sample %d: %v — retrying once; the endpoint is shared and a queued turn is contention, not a finding", name, sample, err)
	}
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
	// targets is the destination each surviving proposal named, counted by value.
	//
	// It is here because a rate of calls says nothing about the one field that
	// decides what happens to the call. `target` is required by the schema and
	// degrades to unsure when it is missing, and unsure is the value that turns the
	// announce-with-Undo path off: capture.Engine.writesPrivateDirectly needs
	// TargetPersonal, so a proposal the model declined to address is a proposal the
	// member is asked about instead of shown. The turn still produces a card, so the
	// call rate above is blind to it — a run can read 20/20 with the whole of D-038
	// switched off underneath.
	//
	// Counted off extractProposal's own Target and not off the raw JSON, for the
	// reason every other number in this file is production's: the question is what
	// the node did with the call, and an omitted field and an unreadable one are the
	// same thing to it.
	targets map[capture.Target]int
	// claimed are replies that said something had been saved. The tool call is a
	// request and the model is never told what became of it, so any such sentence
	// is false at the moment it is written — see claimsASave.
	claimed []string
	// markdown are replies containing Markdown emphasis or fences. Telegram is sent
	// HTML, so these reach the member as the characters the model typed.
	markdown []string
	// bare are the replies on non-calling samples that production now refuses to
	// send as they stand: nothing but an acknowledgement on a turn where nothing
	// happened. It is the same predicate the node runs — lang's, over the member's
	// language — and not a second list written for the scorecard, because a
	// measurement of a guard that is not the guard measures nothing.
	//
	// It is the number this file exists to move. The call rate is the model's
	// judgement and may not be fully fixable by prompting; whether a member who did
	// not get their write is told so is kenward's, and is fixable outright.
	bare []string
	// annotated is the other half of the same accounting: a turn where the member
	// asked, no call arrived, and the reply named a save in so many words rather than
	// acknowledging one. Production leaves these replies standing — the fact is in
	// them — and appends the notice that nothing was recorded. bare and annotated
	// between them are how many of the non-calling turns the member is told the truth
	// on, and the remainder is what somebody has to read.
	annotated []string
	// uncaught is the remainder and the only number in this file nobody can automate:
	// the member asked, no call arrived, and neither guard said anything. If the reply
	// made no claim the member was never misled and the turn is fine; if it made one in
	// words lang.Catalogue.SaveClaims does not hold, the release blocker is back under
	// a new spelling. Kept and printed on its own rather than left to be picked out of
	// the full reply list, because "read them below and check" is an instruction nobody
	// follows on a scorecard of sixteen paragraphs.
	uncaught []string
	// replies is every reply text, kept by TestRequestedCapture and not by
	// TestCaptureJudgement. Thirteen cases times three repeats of unprompted prose is
	// noise nobody reads; four cases where the member asked for a write and may not
	// have got one is the diagnosis itself.
	replies []string
}

// claimsASave reports whether a reply tells the member something has been kept.
//
// It used to be a phrase list living here, on the argument that nothing like it
// belonged in production. That argument was wrong, and a live run said so: once bare
// acknowledgements were replaced, three of seventeen explicit save requests came back
// as "Saved — plumber number is 555 0182." with nothing behind them, which no prompt
// rule and no shape match catches. The list is now
// lang.Catalogue.SaveClaims, the product consults it on every turn that stored
// nothing, and this is the same call — deliberately, because two implementations of
// "does this claim a save" disagree inside a month and this one is what measures
// whether that one works.
//
// It spans SaveClaims and SavePromises both, and that is deliberate. Production gates
// the promises on the member having asked, because outside a request a promise is an
// honest offer; this count is not about the member being misled but about
// docs/PROMPT.md, which asks the model not to talk about the capture at all. "I'll
// remember that" is narration whoever prompted it.
//
// These cases render an English prompt with the default persona, so English is the
// catalogue production would use.
func claimsASave(text string) bool {
	c := lang.For("")
	return c.ClaimsASave(text) || c.PromisesASave(text)
}

// hasMarkdown reports whether a reply carries Markdown the member will read as
// punctuation. Bold and fences only: a single asterisk or underscore appears in
// ordinary prose and in member text often enough that counting it would measure
// nothing.
func hasMarkdown(text string) bool {
	return strings.Contains(text, "**") || strings.Contains(text, "```")
}

// targetTally renders this case's destinations. Per case and not only in aggregate,
// because the right answer differs by scope: a group case can only be shared, and only
// a direct case can be personal — which is the one value announce-with-Undo needs. A
// single total mixes the two and hides a direct scope that never names a destination
// behind a group scope that always does.
func (r result) targetTally() string {
	return fmt.Sprintf("personal=%d shared=%d unsure=%d",
		r.targets[capture.TargetPersonal], r.targets[capture.TargetShared], r.targets[capture.TargetUnsure])
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
	results := make([]result, 0, len(judgementCases))
	for _, c := range judgementCases {
		r := result{c: c, samples: repeats}
		for i := range repeats {
			comp, err := evalComplete(t, ep, c.request(), c.name, i+1)
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
				if r.targets == nil {
					r.targets = map[capture.Target]int{}
				}
				r.targets[p.Target]++
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

// chatCases: sixteen turns of ordinary household conversation with nothing in them to
// keep and nobody asking for anything to be kept.
//
// This is the third population and the newest, and it exists because the other two
// could not see the defect a member reported from a real chat. judgementCases measures
// whether the model spots a durable fact nobody flagged; requestedCases measures what
// it does when somebody asks outright. Neither contains the commonest turn a household
// has — "thanks", "ok", "that's great" — and that is exactly where the truthfulness
// guard was misfiring: the member said "thanks", the model said "Got it.", and the node
// replaced the reply with "I didn't record anything just then. Say it again if you want
// me to remember it."
//
// So these are chosen for the shape that provokes a bare acknowledgement rather than
// for variety: short closings, thanks, agreements, and a few with history so that a
// one-word reply is the natural thing for the model to write. want is false throughout
// and the capture rate is not the point — TestOrdinaryConversation scores something
// else entirely, and a proposal here is a judgementCases-style miss that simply takes
// the sample out of the population, since a turn that stored something is not a turn
// the guard runs on.
var chatCases = []judgementCase{
	{name: "Thanks", history: []turnRecord{{
		user:      "how long do you boil an egg for a soft yolk?",
		assistant: "Six minutes from boiling, then straight into cold water.",
	}}, text: "thanks"},
	{name: "OK", history: []turnRecord{{
		user:      "is the recycling collected fortnightly?",
		assistant: "I don't have anything on the collection schedule, so I can't say.",
	}}, text: "ok"},
	{name: "ThatsGreat", history: []turnRecord{{
		user:      "any ideas for using up a glut of courgettes?",
		assistant: "Fritters, a loaf, or ribboned raw through a lemon salad.",
	}}, text: "that's great, thank you"},
	{name: "PerfectCheers", history: []turnRecord{{
		user:      "what temperature for a slow-roast shoulder of lamb?",
		assistant: "160°C for four hours, covered, then uncovered for the last twenty minutes.",
	}}, text: "perfect, cheers"},
	{name: "GotItThanks", history: []turnRecord{{
		user:      "how do I reset the thermostat?",
		assistant: "Hold the dial in for five seconds until the display blinks, then let go.",
	}}, text: "got it, thanks"},
	{name: "NoWorries", history: []turnRecord{{
		user:      "do you know what time the tip closes?",
		assistant: "I don't — that's not something I have.",
	}}, text: "no worries"},
	{name: "NeverMind", history: []turnRecord{{
		user:      "where did I say the spare fuses were?",
		assistant: "I don't have anything about spare fuses.",
	}}, text: "never mind, I found them"},
	{name: "Morning", text: "morning"},
	{name: "HowAreYou", text: "how are you today?"},
	{name: "SoundsGood", history: []turnRecord{{
		user:      "what should I do with the last of the sourdough starter?",
		assistant: "Crackers — roll it thin with salt and oil and bake it hard.",
	}}, text: "sounds good, I'll try that"},
	{name: "ImOffOut", text: "right, I'm off out for a bit"},
	{name: "ThatsAllForNow", history: []turnRecord{{
		user:      "and what about the oven setting for the crackers?",
		assistant: "200°C, ten minutes, watch them at the end.",
	}}, text: "that's all for now"},
	{name: "PassingMoodAgain", text: "ugh, what a long day"},
	{name: "SmallTalkWeather", text: "it's absolutely bucketing down out there"},
	{name: "AgreeingWithASuggestion", history: []turnRecord{{
		user:      "should I defrost the chicken tonight or in the morning?",
		assistant: "Tonight, in the fridge — morning won't give it long enough.",
	}}, text: "yeah, fair enough"},
	{name: "ThankingForAnAnswerItGaveWrong", history: []turnRecord{{
		user:      "what's the bin day here?",
		assistant: "I don't have anything about bin day in what I can see.",
	}}, text: "ok, no problem"},
}

// TestOrdinaryConversation is the regression a member reported, measured live.
//
// Every sample here is a turn where the member asked for nothing to be kept, and the
// property under test is that none of them shows the truthfulness notice. That is not a
// rate and is not reported as one: unlike the capture decision, which depends on the
// model and the sampler and the weather, whether the node speaks over an ordinary reply
// is entirely kenward's and has one right answer. One is too many, and one is what the
// member saw.
//
// It runs production's own falseSave against the real reply, so what is counted is what
// a member would have read. Samples where the model made a tool call are excluded and
// counted separately: the guard does not run on a turn that did something, so those
// samples say nothing about it either way.
//
// Two counts are printed whether or not anything failed, because a clean sheet on its
// own is not evidence. The bare-reply count says whether the run reached the arm this
// population exists for: a run in which the model never once answered with a bare
// acknowledgement has not tested it. And the count of samples the *pre-change* guard
// would have spoken over — `IsBareAcknowledgement || ClaimsASave`, which is what the
// switch used to be — is the size of the defect on this population. Zero after is only
// worth reading next to what it was before.
func TestOrdinaryConversation(t *testing.T) {
	ep := evalEndpoint(t)
	repeats := evalRepeats(t)
	cat := lang.For("")

	var total, acted, empty, bareReplies int
	var notices, before []string
	t.Logf("\n=== ordinary conversation: %s @ %s, %d repeats ===", ep.Model, ep.BaseURL, repeats)
	for _, c := range chatCases {
		for i := range repeats {
			comp, err := evalComplete(t, ep, c.request(), c.name, i+1)
			var emptyErr *llm.EmptyResponseError
			switch {
			case errors.As(err, &emptyErr):
				empty++
				continue
			case err != nil:
				t.Fatalf("%s sample %d: completing against %s (%s): %v", c.name, i+1, ep.BaseURL, ep.Model, err)
			}
			if len(comp.ToolCalls) > 0 {
				// The model decided this turn was worth storing. That is a judgement
				// question and judgementCases is where it is scored; here it only means
				// the guard never runs, so the sample leaves the population.
				acted++
				t.Logf("  [%s] called %d tool(s) — excluded, the guard does not run on a turn that acted", c.name, len(comp.ToolCalls))
				continue
			}
			total++
			reply, _ := sanitizeReply(comp.Text)
			bare := cat.IsBareAcknowledgement(reply)
			if bare {
				bareReplies++
			}
			// What the guard did before the SaveRequests gate: the bare arm fired on
			// any acknowledgement, whatever the member had said, and replaced the reply
			// with the notice; the claim arm held the promise vocabulary and fired on
			// that unconditionally too. claimsASave spans both tables, so this is the
			// old condition exactly. Every entry here is a turn where the member would
			// have been told to repeat a request they never made.
			if bare || claimsASave(reply) {
				before = append(before, fmt.Sprintf("member said %q, model said %q", c.text, oneLine(reply)))
			}
			if falseSave(cat, c.text, reply) {
				notices = append(notices, fmt.Sprintf("member said %q, model said %q", c.text, oneLine(reply)))
			}
			t.Logf("  [%-30s] %q", c.name, oneLine(reply))
		}
	}

	t.Logf("  ordinary turns that stored nothing   %d", total)
	t.Logf("    of those, a bare acknowledgement   %d", bareReplies)
	t.Logf("    the old guard would have spoken over  %d", len(before))
	for _, s := range before {
		t.Logf("      was: %s", s)
	}
	t.Logf("    shown the notice now               %d", len(notices))
	if acted > 0 {
		t.Logf("  samples excluded for calling a tool  %d", acted)
	}
	if empty > 0 {
		t.Logf("  samples the endpoint answered with nothing at all: %d", empty)
	}
	if bareReplies == 0 {
		t.Logf("  no sample produced a bare acknowledgement, so the arm this population exists for was never reached — the zero above means less than it looks")
	}
	if len(notices) > 0 {
		t.Errorf("%d ordinary turn(s) told the member nothing was recorded. They asked for nothing to be kept, so there was nothing to correct, and the notice invites them to repeat a request they never made:", len(notices))
		for _, s := range notices {
			t.Errorf("    %s", s)
		}
	}
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
