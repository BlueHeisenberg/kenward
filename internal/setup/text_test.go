package setup

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got against testdata/name, or rewrites it under -update.
//
// The three golden files in this package are the trust question and the two privacy
// statements. They are golden precisely because they are the sentences somebody
// makes a decision on: changing one has to be a deliberate edit to a fixture that
// somebody reviews, never a diff nobody notices inside a refactor.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (run go test -update to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s has changed.\n\n--- want ---\n%s\n--- got ---\n%s\n\n"+
			"If the change is deliberate, run: go test ./internal/setup -update", name, want, got)
	}
}

func TestTrustQuestionGolden(t *testing.T) {
	golden(t, "trust_question.txt",
		renderChoice(TrustQuestion, []string{trustAnswerSimple, trustAnswerIsolated}, -1))
}

// TestTrustQuestionMatchesCLIDoc asserts that what the wizard prints is what
// docs/CLI.md says it prints, character for character.
//
// The document is the specification for this command and was written before the
// code. Letting the two drift would mean the specification quietly became a
// description of something else, so the test reads the document rather than a copy
// of it.
func TestTrustQuestionMatchesCLIDoc(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "CLI.md"))
	if err != nil {
		t.Skipf("docs/CLI.md is not readable from here: %v", err)
	}
	want := extractTrustBlock(t, string(doc))
	// The rendered form ends with the prompt line, which the document does not
	// show, so the comparison is of everything above it.
	rendered := renderChoice(TrustQuestion, []string{trustAnswerSimple, trustAnswerIsolated}, -1)
	got := strings.TrimRight(strings.TrimSuffix(rendered, renderChoicePrompt(2, -1)), " \n")
	if got != want {
		t.Errorf("the trust question no longer matches docs/CLI.md.\n\n--- CLI.md ---\n%s\n--- wizard ---\n%s", want, got)
	}
}

// extractTrustBlock pulls the fenced block out of the `kenward setup` section of
// CLI.md and removes the three spaces of list indentation it sits under.
func extractTrustBlock(t *testing.T, doc string) string {
	t.Helper()
	const marker = "Does everyone in this household trust"
	start := strings.Index(doc, marker)
	if start < 0 {
		t.Fatalf("docs/CLI.md no longer contains the trust question")
	}
	rest := doc[start:]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatalf("docs/CLI.md's trust question block is not closed")
	}
	var lines []string
	for _, line := range strings.Split(rest[:end], "\n") {
		lines = append(lines, strings.TrimPrefix(line, "   "))
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \n")
}

func TestPrivacyStatementGolden(t *testing.T) {
	golden(t, "privacy_simple.txt", PrivacyStatement(config.ModeSimple))
	golden(t, "privacy_isolated.txt", PrivacyStatement(config.ModeIsolated))
}

// TestPrivacyStatementSimpleIsNotSoftened guards the specific sentence the whole
// mode rests on, in the specific words ARCHITECTURE.md uses, and guards against the
// sealed-memory vocabulary that CLAUDE.md forbids for simple mode.
func TestPrivacyStatementSimpleIsNotSoftened(t *testing.T) {
	got := PrivacyStatement(config.ModeSimple)

	for _, want := range []string{
		"Whoever runs this machine can read every member's private\n  memory",
		"Separation between members is real",
		"sealing\n  against the operator is not",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the simple-mode privacy statement no longer contains %q", want)
		}
	}
	for _, forbidden := range []string{"sealed", "end-to-end", "encrypted at rest", "cannot read"} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Errorf("the simple-mode privacy statement claims %q, which simple mode does not do", forbidden)
		}
	}
}

// TestPrivacyStatementIsolatedClaimsNoMore checks the isolated statement makes the
// claim ARCHITECTURE.md actually supports and not the stronger one.
func TestPrivacyStatementIsolatedClaimsNoMore(t *testing.T) {
	got := PrivacyStatement(config.ModeIsolated)
	for _, want := range []string{
		"The claim is not that the operator cannot read your memory.",
		"Root always wins.",
		"kenward can read private memory at all.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the isolated privacy statement no longer contains %q", want)
		}
	}
}

// TestBothStatementsStateTheRoutingGuarantee checks that the sentence describing
// what routing will never do appears in both, because it is the one privacy
// property that does not depend on the mode.
func TestBothStatementsStateTheRoutingGuarantee(t *testing.T) {
	const guarantee = "never reaches a provider"
	for _, mode := range []config.Mode{config.ModeSimple, config.ModeIsolated} {
		if !strings.Contains(PrivacyStatement(mode), guarantee) {
			t.Errorf("the %s privacy statement does not state the routing guarantee", mode)
		}
	}
}

func TestOSName(t *testing.T) {
	for goos, want := range map[string]string{
		"windows": "Windows",
		"darwin":  "macOS",
		"linux":   "Linux",
		"plan9":   "plan9",
	} {
		if got := osName(goos); got != want {
			t.Errorf("osName(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestFormatList(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, "a provider"},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b and c"},
	} {
		if got := formatList(tc.in); got != tc.want {
			t.Errorf("formatList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPadCountsRunesNotBytes(t *testing.T) {
	if got := pad("María", 6); got != "María " {
		t.Errorf("pad(María, 6) = %q, want one trailing space", got)
	}
	if got := pad("David", 6); got != "David " {
		t.Errorf("pad(David, 6) = %q, want one trailing space", got)
	}
}

// TestLinesAreReadable keeps the copy inside a width somebody can read in a
// terminal that has not been resized. Long lines are how a carefully written
// paragraph turns into a wall.
func TestLinesAreReadable(t *testing.T) {
	blocks := map[string]string{
		"Banner":                 Banner,
		"TrustQuestion":          TrustQuestion,
		"privacySimple":          privacySimple,
		"privacyIsolated":        privacyIsolated,
		"isolatedNeedsLinux":     isolatedNeedsLinux("windows"),
		"membersIntro":           membersIntro,
		"endpointsIntro":         endpointsIntro,
		"tiersIntro":             tiersIntro,
		"telegramIntroSimple":    telegramIntroSimple,
		"telegramIntroIsolated":  telegramIntroIsolated,
		"botFatherWalkthrough":   botFatherWalkthrough,
		"tiersNoLocalWarning":    tiersNoLocalWarning,
		"stoppedForLinux":        stoppedForLinux,
		"stoppedNoLocal":         stoppedNoLocal,
		"envFileNote":            envFileNote,
		"tokenNotStored":         tokenNotStored("KENWARD_BOT_TOKEN"),
		"cloudConsequence":       cloudConsequence("David's private messages", []string{"cloud"}, []string{"openrouter.ai"}),
		"privateDefaultNote":     privateDefaultNote("David", []string{"local"}),
		"groupDefaultNote":       groupDefaultNote([]string{"local"}),
		"sharedSpaceNote":        sharedSpaceNote,
		"endpointTiersNote":      endpointTiersNote,
		"householdIntro":         householdIntro,
		"tokenLooksWrongMessage": tokenLooksWrong,
	}
	const limit = 82
	for name, block := range blocks {
		for i, line := range strings.Split(block, "\n") {
			if n := len([]rune(line)); n > limit {
				t.Errorf("%s line %d is %d columns wide:\n%s", name, i+1, n, line)
			}
		}
	}
}
