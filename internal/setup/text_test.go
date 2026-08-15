package setup

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got against testdata/name, or rewrites it under -update.
//
// The golden file in this package is the trust question, which is golden because it
// is the sentence somebody makes an irreversible decision on: changing it has to be
// a deliberate edit to a fixture that somebody reviews, never a diff nobody notices
// inside a refactor. The privacy statements are golden too, in internal/privacy,
// where they now live.
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

// TestPrivacyBlockPrintsTheSharedStatementVerbatim is the assertion that keeps this
// package out of the business of describing what kenward protects.
//
// The wording lives in internal/privacy, golden-tested there, and is printed here
// unchanged. A wizard that reworded it — even improved it — would be a second copy
// of a promise, and the way a promise like this decays is one copy being softened
// while the other is not.
func TestPrivacyBlockPrintsTheSharedStatementVerbatim(t *testing.T) {
	for _, tc := range []struct {
		mode    config.Mode
		want    privacy.Mode
		heading string
	}{
		{config.ModeSimple, privacy.ModeSimple, "Privacy, in simple mode"},
		{config.ModeIsolated, privacy.ModeIsolated, "Privacy, in isolated mode"},
	} {
		block := privacyBlock(tc.mode)
		if !strings.HasPrefix(block, tc.heading) {
			t.Errorf("%s block does not open with %q", tc.mode, tc.heading)
		}
		if !strings.Contains(block, privacy.Statement(tc.want)) {
			t.Errorf("the %s block does not contain internal/privacy's statement verbatim:\n%s", tc.mode, block)
		}
		if strings.Contains(block, privacy.Statement(otherMode(tc.want))) {
			t.Errorf("the %s block contains the other mode's statement", tc.mode)
		}
		if !strings.Contains(block, "kenward doctor prints this same statement") {
			t.Errorf("the %s block does not say where the reader will see this again", tc.mode)
		}
	}
}

func otherMode(m privacy.Mode) privacy.Mode {
	if m == privacy.ModeSimple {
		return privacy.ModeIsolated
	}
	return privacy.ModeSimple
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
		"privacyTrailer":         privacyTrailer,
		"systemdNote":            systemdNote,
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
