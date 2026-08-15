package setup

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestConsoleAndScriptRenderIdentically is what makes every other test in this
// package worth anything.
//
// The scripted fake is only evidence about the wizard if it shows a person exactly
// what a terminal would. Both implementations sit on the same rendering and parsing
// code, and this asserts that they have not been allowed to diverge.
func TestConsoleAndScriptRenderIdentically(t *testing.T) {
	answers := []string{"Casa", "2", "y", "secret"}
	exercise := func(io IO) {
		_, _ = io.Ask("What is this household called?", "Home")
		_, _ = io.AskChoice(TrustQuestion, []string{trustAnswerSimple, trustAnswerIsolated}, -1)
		_, _ = io.AskYesNo("Write the file?", true)
		_, _ = io.AskSecret("Bot token")
		io.Print("done")
	}

	var out bytes.Buffer
	exercise(NewConsoleIO(strings.NewReader(strings.Join(answers, "\n")+"\n"), &out))

	script := NewScriptIO(answers...)
	exercise(script)

	// The console echoes nothing itself — the terminal does that — so the fake's
	// transcript carries the answers and the console's output does not. Comparing
	// the prompts is the point.
	consolePrompts := out.String()
	for _, want := range []string{
		"What is this household called? [Home] ",
		"  [1] " + trustAnswerSimple,
		"Choose 1 or 2: ",
		"Write the file? [Y/n] ",
		"Bot token: ",
		"done",
	} {
		if !strings.Contains(consolePrompts, want) {
			t.Errorf("the console did not print %q", want)
		}
		if !strings.Contains(script.Transcript(), want) {
			t.Errorf("the fake did not print %q", want)
		}
	}
}

func TestAskUsesTheDefaultOnEmptyInput(t *testing.T) {
	io := NewConsoleIO(strings.NewReader("\n"), &bytes.Buffer{})
	got, err := io.Ask("Name", "Home")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Home" {
		t.Errorf("Ask = %q, want the default", got)
	}
}

func TestAskChoiceWithoutADefaultKeepsAsking(t *testing.T) {
	var out bytes.Buffer
	io := NewConsoleIO(strings.NewReader("\nbanana\n7\n2\n"), &out)
	got, err := io.AskChoice("Pick", []string{"one", "two"}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("AskChoice = %d, want 1", got)
	}
	if n := strings.Count(out.String(), "Please answer with 1 or 2."); n != 3 {
		t.Errorf("the wizard nudged %d times, want 3 (empty, word, out of range)", n)
	}
	// Re-asking must not scroll the options off the screen of somebody trying to
	// read them.
	if n := strings.Count(out.String(), "[1] one"); n != 1 {
		t.Errorf("the options were reprinted %d times", n)
	}
}

func TestAskChoiceWithADefaultTakesEnter(t *testing.T) {
	io := NewConsoleIO(strings.NewReader("\n"), &bytes.Buffer{})
	got, err := io.AskChoice("Pick", []string{"one", "two"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("AskChoice = %d, want the default", got)
	}
}

func TestAskYesNo(t *testing.T) {
	for in, want := range map[string]bool{
		"y\n": true, "Y\n": true, "yes\n": true, "YES\n": true,
		"n\n": false, "no\n": false, "\n": false,
	} {
		io := NewConsoleIO(strings.NewReader(in), &bytes.Buffer{})
		got, err := io.AskYesNo("Well?", false)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Errorf("AskYesNo(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAskYesNoRepeatsUntilItGetsAnAnswer(t *testing.T) {
	var out bytes.Buffer
	io := NewConsoleIO(strings.NewReader("maybe\nsure\ny\n"), &out)
	got, err := io.AskYesNo("Well?", false)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("AskYesNo = false")
	}
	if n := strings.Count(out.String(), "Please answer yes or no."); n != 2 {
		t.Errorf("nudged %d times, want 2", n)
	}
}

// TestAskSecretWithoutATerminal covers the case every test in this package runs in,
// and the case a scripted install runs in: input that is not a terminal has no echo
// to suppress, and the read must work rather than refuse.
func TestAskSecretWithoutATerminal(t *testing.T) {
	var out bytes.Buffer
	io := NewConsoleIO(strings.NewReader("  123:abc  \n"), &out)
	got, err := io.AskSecret("Bot token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "123:abc" {
		t.Errorf("AskSecret = %q, want it trimmed", got)
	}
}

func TestInputEndingIsReportedAsSuch(t *testing.T) {
	io := NewConsoleIO(strings.NewReader(""), &bytes.Buffer{})
	if _, err := io.Ask("Name", ""); !errors.Is(err, ErrInputClosed) {
		t.Errorf("err = %v, want ErrInputClosed", err)
	}
}

// TestALastLineWithoutANewlineIsStillAnAnswer covers a here-document or a pasted
// block that does not end in a newline.
func TestALastLineWithoutANewlineIsStillAnAnswer(t *testing.T) {
	io := NewConsoleIO(strings.NewReader("Casa"), &bytes.Buffer{})
	got, err := io.Ask("Name", "Home")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Casa" {
		t.Errorf("Ask = %q", got)
	}
}

func TestCarriageReturnsAreStripped(t *testing.T) {
	io := NewConsoleIO(strings.NewReader("Casa\r\n"), &bytes.Buffer{})
	got, err := io.Ask("Name", "Home")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Casa" {
		t.Errorf("Ask = %q, want the carriage return gone", got)
	}
}

func TestScriptIOReportsRunningOut(t *testing.T) {
	io := NewScriptIO("one")
	if _, err := io.Ask("First", ""); err != nil {
		t.Fatal(err)
	}
	if io.Remaining() != 0 {
		t.Errorf("Remaining = %d", io.Remaining())
	}
	if _, err := io.Ask("Second", ""); !errors.Is(err, ErrInputClosed) {
		t.Errorf("err = %v, want ErrInputClosed", err)
	}
}

// TestScriptIONeverLogsASecret: the transcript is printed on test failure and
// pasted into bug reports.
func TestScriptIONeverLogsASecret(t *testing.T) {
	io := NewScriptIO("hunter2")
	if _, err := io.AskSecret("Bot token"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(io.Transcript(), "hunter2") {
		t.Error("the fake logged a value read as a secret")
	}
}

func TestRenderQuestion(t *testing.T) {
	for _, tc := range []struct{ question, def, want string }{
		{"What is it called?", "Home", "What is it called? [Home] "},
		{"What is it called?", "", "What is it called? "},
		{"Bot token", "", "Bot token: "},
		{"Environment variable", "KENWARD_BOT_TOKEN", "Environment variable [KENWARD_BOT_TOKEN] "},
	} {
		if got := renderQuestion(tc.question, tc.def); got != tc.want {
			t.Errorf("renderQuestion(%q, %q) = %q, want %q", tc.question, tc.def, got, tc.want)
		}
	}
}

func TestChoiceRange(t *testing.T) {
	if got := choiceRange(2); got != "1 or 2" {
		t.Errorf("choiceRange(2) = %q", got)
	}
	if got := choiceRange(4); got != "a number from 1 to 4" {
		t.Errorf("choiceRange(4) = %q", got)
	}
}
