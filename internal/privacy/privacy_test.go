package privacy

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// TestStatementsAreGolden pins the privacy statements against accidental softening.
//
// These are not ordinary strings. They are the point at which a claim becomes checkable,
// and the failure mode this guards against is not a typo — it is someone tidying an
// uncomfortable sentence into a comfortable one. Every assertion below corresponds to a
// promise the product either keeps or must stop making.
func TestStatementsAreGolden(t *testing.T) {
	t.Parallel()

	t.Run("simple mode admits the operator can read everything", func(t *testing.T) {
		t.Parallel()
		s := Statement(ModeSimple)

		mustContain(t, s, "can read every member's private memory",
			"simple mode must state the operator's reach in plain words")
		mustContain(t, s, "does NOT",
			"the limitation must be emphatic, not buried in a subordinate clause")
		mustContain(t, s, "separate",
			"member-to-member separation is real in this mode and should be claimed")

		// ARCHITECTURE.md's key-custody section says the operator's reach in this
		// mode covers memory at rest *and* in flight, because one bot token carries
		// every conversation. A statement that admitted only the disk would leave a
		// reader believing the channel was protected.
		mustContain(t, s, "in flight",
			"the single household bot exposes conversations in flight, not only at rest")

		// The sealing vocabulary belongs to isolated mode alone. If it appears here,
		// the two modes' promises have been conflated, which is the specific error
		// the product documentation warns about repeatedly.
		mustNotContain(t, s, "sealed against",
			"simple mode must never borrow isolated mode's sealing language")
	})

	t.Run("isolated mode states the limit as prominently as the claim", func(t *testing.T) {
		t.Parallel()
		s := Statement(ModeIsolated)

		mustContain(t, s, "own key", "the per-member key is the mechanism and should be named")
		mustContain(t, s, "not from a backup", "at-rest protection is the strongest true claim")
		mustContain(t, s, "honest limit", "the bound must be labelled, not implied")
		mustContain(t, s, "root access", "the residual risk must name what defeats it")
		mustContain(t, s, "plain text", "the reason the limit exists must be stated, not asserted")

		// D-019: passphrases never travel over Telegram, and the statement must say
		// so, because the alternative reading — that unlocking happens in the chat —
		// would teach exactly the habit the design refuses to support.
		mustContain(t, s, "never travels over Telegram",
			"the passphrase channel decision must be visible to the member")

		// The first thing a privacy-minded reader checks: the node can read the
		// private space at all, because it is the second member of it. Leaving that
		// implicit would make the rest of the claim read as stronger than it is.
		mustContain(t, s, "second member of your private space",
			"the node's own access to a private space must be stated, not implied")

		// The claim that was removed because it could not be honoured. If it ever
		// returns, the statement is promising idle-locking that does not exist.
		mustNotContain(t, s, "while you are away",
			"this claim was withdrawn in D-019 and must not reappear")

		// M-13: the text once stated the no-expiry behaviour as fact while
		// internal/session zeroed keys after thirty minutes. The statement is
		// printed by `kenward doctor` without knowing the setting, so it has to
		// hold for a household that turned expiry on as well as one that did not.
		// Naming the knob is what makes it hold; delete the name and the sentence
		// silently becomes a promise again.
		mustContain(t, s, "session.idle_timeout",
			"the idle knob must be named, or the statement is only true for its default")
		mustContain(t, s, "it is off unless someone did",
			"the default must be stated, since a member cannot read the configuration")
		mustContain(t, s, "stops answering until somebody at the machine starts it again",
			"a household that turns expiry on must be told there is no in-band way back")
	})

	t.Run("both modes state the guarantees that do not depend on topology", func(t *testing.T) {
		t.Parallel()
		for _, m := range []Mode{ModeSimple, ModeIsolated} {
			s := Statement(m)
			// Routing is the one privacy property a reader can check against their
			// own configuration, and it holds in both modes. If a statement stops
			// making the promise, either routing has changed or the statement has.
			mustContain(t, s, "never reaches a provider",
				"the local-only routing guarantee must be stated in "+m.String()+" mode")
			mustContain(t, s, "nothing is written to memory without the member",
				"capture consent must be stated in "+m.String()+" mode")
		}
	})

	t.Run("an unknown mode has no statement", func(t *testing.T) {
		t.Parallel()
		if got := Statement(ModeUnknown); got != "" {
			t.Errorf("Statement(ModeUnknown) = %q, want empty; rendering a privacy claim for an unknown mode is a bug, not a default", got)
		}
		if got := Statement(Mode(99)); got != "" {
			t.Errorf("Statement(Mode(99)) = %q, want empty", got)
		}
	})

	t.Run("statements have no leading or trailing blank lines", func(t *testing.T) {
		t.Parallel()
		for _, m := range []Mode{ModeSimple, ModeIsolated} {
			s := Statement(m)
			if s != strings.TrimSpace(s) {
				t.Errorf("Statement(%s) has surrounding whitespace; callers compose it into wizard and doctor output", m)
			}
		}
	})
}

func TestModeString(t *testing.T) {
	t.Parallel()

	for mode, want := range map[Mode]string{
		ModeUnknown:  "unknown",
		ModeSimple:   "simple",
		ModeIsolated: "isolated",
		Mode(42):     "unknown",
	} {
		if got := mode.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", mode, got, want)
		}
	}
}

func TestTierNote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		label string
		tiers []string
		local bool
		want  string
	}{
		{
			"a local-only chain promises refusal",
			"david", []string{"local"}, true,
			"david: [local] — will refuse rather than use a provider",
		},
		{
			"a chain reaching a provider says so",
			"household", []string{"local", "cloud"}, false,
			"household: [local, cloud] — may use a provider",
		},
		{
			// Configuration validation rejects an empty chain, so reaching this
			// means a Scope was built by hand. Say something true rather than
			// something reassuring.
			"no tiers is reported as refusing everything",
			"david", nil, false,
			"david: no tiers configured — every request will be refused",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := TierNote(tc.label, tc.tiers, tc.local); got != tc.want {
				t.Errorf("TierNote() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestMemberNote(t *testing.T) {
	t.Parallel()

	m := domain.Member{ID: "david", Name: "David", Tiers: []string{"local"}}
	want := "David: [local] — will refuse rather than use a provider"
	if got := MemberNote(m, true); got != want {
		t.Errorf("MemberNote() = %q, want %q", got, want)
	}
}

// flatten collapses all runs of whitespace to single spaces so that assertions match
// phrases rather than line breaks. The statements are hard-wrapped prose, and a test
// that fails when a paragraph is re-flowed would train people to weaken the assertion
// instead of preserving the promise — which is the opposite of what these tests are for.
func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

func mustContain(t *testing.T, haystack, needle, why string) {
	t.Helper()
	if !strings.Contains(flatten(haystack), flatten(needle)) {
		t.Errorf("statement is missing %q — %s", needle, why)
	}
}

func mustNotContain(t *testing.T, haystack, needle, why string) {
	t.Helper()
	if strings.Contains(flatten(haystack), flatten(needle)) {
		t.Errorf("statement contains %q — %s", needle, why)
	}
}
