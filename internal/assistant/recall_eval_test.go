//go:build integration

package assistant

// The recall population: a member asks about something the household has already
// recorded, and the entry is right there in the prompt.
//
// This is the fourth population and the one the other three could not see. judgementCases
// puts a durable fact in front of the model and asks whether it notices; requestedCases
// asks for a write outright; chatCases is ordinary conversation with nothing in it at
// all. None of them is the turn a household actually has most days after the first week
// — "what's the wifi password?" — and that turn is where the truthfulness guard was
// found misfiring, on a real Telegram chat:
//
//	🔍 searched your private memory (nothing), the household memory (1 entry)
//	It's heron-ashfield-42, at least as of the last time it was written down —
//	worth double-checking if it doesn't connect, since passwords change.
//	⚠️ I didn't record anything just then. Say it again if you want me to remember it.
//
// Nothing about that turn is wrong except the last line. The guard fired because the
// answer said "written down", which was on the arm that runs with no gate in front of
// it — and "written down" is how a recall dates the entry it just read. The same held
// for "it's in your memory", "I have it", "added to your private memory back in March".
// The guard was loudest exactly where the product worked.
//
// So the property here is the one TestOrdinaryConversation asserts on its own
// population, on the turns that matter more: a member who asked a question and got a
// true answer out of memory is never told nothing was recorded. It is not a rate. It has
// one right answer, it is entirely kenward's, and one is too many.
//
// Two numbers are printed whether or not anything fails, for the reason
// TestOrdinaryConversation prints two: zero after is worth reading only next to what it
// was before. The "before" column runs production's own falseSave over a catalogue whose
// unmistakable list has been rebuilt the way the code derived it until this change —
// SaveClaims less every entry BareAcknowledgements also holds — so the comparison is the
// real function on the real replies, and not a description of the old rule.
//
//	KENWARD_E2E_ENDPOINT=http://127.0.0.1:8000/v1 \
//	KENWARD_E2E_MODEL=qwen3.8-27b-dflash2 \
//	KENWARD_EVAL_REPEATS=1 \
//	go test -tags integration -run TestRecall -v ./internal/assistant/

import (
	"errors"
	"fmt"
	"testing"

	"github.com/BlueHeisenberg/keel/llm"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/memory"
)

// recallEntry is the shape every entry below takes; only the space, title and body
// differ, and writing them out twenty times would bury what each case is asking.
func recallEntry(space domain.SpaceID, title, body string) memory.Entry {
	return memory.Entry{
		Space: space, Domain: "household/logistics",
		Title: title, Body: body, Confidence: "validated",
	}
}

func privateEntry(title, body string) memory.Entry { return recallEntry("david-private", title, body) }
func sharedEntry(title, body string) memory.Entry  { return recallEntry("household", title, body) }

// recallCases: twenty turns where the member asks about something the household has
// recorded and the entry is in the prompt.
//
// want is false throughout and the capture rate is not the point — a model that decides
// to re-store what it just read is failing docs/PROMPT.md's "anything already in the
// memory shown above", which judgementCases scores. Here a proposal only takes the
// sample out of the population, because the guard does not run on a turn that acted.
//
// The questions are written the way a household writes them: no keyword the entry's
// title uses twice, some with the answer in the shared space and some in the private
// one, and several inviting the model to say when the thing was written down — which is
// the phrasing that fired.
var recallCases = []judgementCase{
	{
		name:   "WifiPassword",
		shared: []memory.Entry{sharedEntry("Wifi password", "The wifi password is heron-ashfield-42.")},
		text:   "what's the wifi password?",
	},
	{
		name:   "WifiPasswordStillCurrent",
		shared: []memory.Entry{sharedEntry("Wifi password", "The wifi password is heron-ashfield-42.")},
		text:   "is the wifi password still the same one as before?",
	},
	{
		name:   "BoilerCode",
		shared: []memory.Entry{sharedEntry("Boiler service code", "The boiler service code is 4471.")},
		text:   "do we have the boiler service code anywhere?",
	},
	{
		name:   "Stopcock",
		shared: []memory.Entry{sharedEntry("Stopcock", "The stopcock is under the stairs.")},
		text:   "where's the stopcock?",
	},
	{
		name:    "PlumberNumber",
		private: []memory.Entry{privateEntry("Plumber", "The plumber's number is 555 0182.")},
		text:    "have you got the plumber's number?",
	},
	{
		name:    "SpareKey",
		private: []memory.Entry{privateEntry("Spare key", "The spare key lives under the third plant pot by the shed.")},
		text:    "where did I say the spare key was?",
	},
	{
		name:   "BinDay",
		shared: []memory.Entry{sharedEntry("Bin day", "The bins go out on Thursday night.")},
		text:   "which night do the bins go out?",
	},
	{
		name:   "AlarmCode",
		shared: []memory.Entry{sharedEntry("Alarm code", "The house alarm code is 8812.")},
		text:   "remind me what the alarm code is",
	},
	{
		name:    "DentistAppointment",
		private: []memory.Entry{privateEntry("Dentist", "Appointment on the first Monday of October.")},
		text:    "when's my dentist appointment again?",
	},
	{
		name:   "MeterReadingDay",
		shared: []memory.Entry{sharedEntry("Meter readings", "Readings are submitted on the 28th of each month.")},
		text:   "what day do we submit the meter readings?",
	},
	{
		name:   "VetAddress",
		shared: []memory.Entry{sharedEntry("Vet", "The vet is on Mill Lane, open until six on weekdays.")},
		text:   "where's the vet again, and how late are they open?",
	},
	{
		name:    "DairyFree",
		private: []memory.Entry{privateEntry("Dairy free", "David has been dairy-free for years.")},
		text:    "can I have the carbonara recipe you mentioned?",
	},
	{
		name:   "ScreenRule",
		shared: []memory.Entry{sharedEntry("Screens", "No screens after eight on school nights.")},
		text:   "what did we agree about screens on school nights?",
	},
	{
		name:   "SesameAllergy",
		shared: []memory.Entry{sharedEntry("Sam's allergy", "Sam cannot have sesame; it brings him out in hives.")},
		text:   "Sam's coming for dinner — is there anything he can't eat?",
	},
	{
		name:   "OilTankRefill",
		shared: []memory.Entry{sharedEntry("Oil tank", "The oil tank is refilled every September by Ashfield Fuels.")},
		text:   "who fills the oil tank and when?",
	},
	{
		name:    "GymSchedule",
		private: []memory.Entry{privateEntry("Gym", "David goes to the gym on Tuesday and Thursday at seven.")},
		text:    "what days am I at the gym?",
	},
	{
		name: "WhenWasItWrittenDown",
		shared: []memory.Entry{
			sharedEntry("Wifi password", "The wifi password is heron-ashfield-42."),
			sharedEntry("Boiler service code", "The boiler service code is 4471."),
		},
		text: "how long have you had the wifi password for?",
	},
	{
		name:    "DoYouStillHaveIt",
		private: []memory.Entry{privateEntry("Plumber", "The plumber's number is 555 0182.")},
		text:    "do you still have the plumber's number?",
	},
	{
		name:   "GroupAsksForTheCode",
		group:  true,
		shared: []memory.Entry{sharedEntry("Boiler service code", "The boiler service code is 4471.")},
		text:   "does anyone know the boiler service code? kenward?",
	},
	{
		name:   "GroupAsksWhereTheKeyIs",
		group:  true,
		shared: []memory.Entry{sharedEntry("Spare key", "The spare key lives under the third plant pot by the shed.")},
		text:   "kenward, where's the spare key kept?",
	},
}

// preSplitCatalogue rebuilds the unmistakable list the way the code derived it before
// the recall defect: every save claim that is not also one of this language's bare
// acknowledgements, matched as a substring.
//
// It exists so the "before" column in the run below is production's own falseSave over
// a different table, rather than a second implementation of the old rule written from
// its documentation. A copy of a guard measures the copy — the same argument that moved
// SaveClaims out of this package's evals and into lang.
func preSplitCatalogue(c lang.Catalogue) lang.Catalogue {
	var out []string
	for _, p := range c.SaveClaims {
		if c.IsBareAcknowledgement(p) {
			continue
		}
		out = append(out, p)
	}
	c.UnmistakableSaveClaims = out
	return c
}

// TestRecall is the defect a member reported, measured live: does a correct answer out
// of memory ever carry "I didn't record anything just then"?
//
// It fails on a completion error and on any notice at all. It does not fail on the
// capture rate, on the answer's accuracy, or on whether the model found the entry —
// those are judgementCases' business and a reader's. What is asserted is the one thing
// that is entirely the node's: it does not speak over a right answer.
func TestRecall(t *testing.T) {
	ep := evalEndpoint(t)
	repeats := evalRepeats(t)
	cat := lang.For("")
	before := preSplitCatalogue(cat)

	var total, acted, empty int
	var now, was []string
	t.Logf("\n=== recall: %s @ %s, %d repeats ===", ep.Model, ep.BaseURL, repeats)
	for _, c := range recallCases {
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
				acted++
				t.Logf("  [%s] called %d tool(s) — excluded, the guard does not run on a turn that acted", c.name, len(comp.ToolCalls))
				continue
			}
			total++
			reply, _ := sanitizeReply(comp.Text)
			line := fmt.Sprintf("member asked %q, model answered %q", c.text, oneLine(reply))
			if falseSave(before, c.text, reply) {
				was = append(was, line)
			}
			if falseSave(cat, c.text, reply) {
				now = append(now, line)
			}
			t.Logf("  [%-30s] %q", c.name, oneLine(reply))
		}
	}

	t.Logf("  recall turns that stored nothing      %d", total)
	t.Logf("    the derived guard would have spoken over  %d", len(was))
	for _, s := range was {
		t.Logf("      was: %s", s)
	}
	t.Logf("    shown the notice now                %d", len(now))
	if acted > 0 {
		t.Logf("  samples excluded for calling a tool   %d", acted)
	}
	if empty > 0 {
		t.Logf("  samples the endpoint answered with nothing at all: %d", empty)
	}
	if len(now) > 0 {
		t.Errorf("%d recall turn(s) told the member nothing was recorded. They asked a question, the answer came out of memory, and the notice denies the one thing kenward is for:", len(now))
		for _, s := range now {
			t.Errorf("    %s", s)
		}
	}
}
