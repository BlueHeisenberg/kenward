package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ErrInputClosed is returned when the wizard asks a question and there is nobody
// left to answer it — stdin at end of file, or a script that ran out of answers.
//
// It is a sentinel so that `cmd/kenward` can tell "the operator pressed Ctrl-D" from
// "setup failed", and exit quietly rather than printing a failure at somebody who
// simply changed their mind.
var ErrInputClosed = errors.New("setup: input ended before the wizard finished")

// IO is everything the wizard needs from the person running it.
//
// It is an interface for one reason that matters more than testability: no test in
// this package may require a terminal. A wizard whose flow can only be exercised by
// a human typing at it is a wizard whose wording is never checked, and the wording
// is the part of setup most likely to be wrong.
//
// Implementations render questions identically — ConsoleIO and ScriptIO share the
// rendering and parsing below — so a transcript captured from the fake is what a
// person actually sees.
type IO interface {
	// Print writes a block of text followed by a newline. Blocks may be multi-line
	// and arrive pre-wrapped; nothing re-flows them to the terminal width, because
	// the alignment in several of them is load-bearing.
	Print(text string)

	// Ask puts a free-text question. An empty answer selects def; when def is empty
	// an empty answer is returned as such and it is the caller's business whether
	// that is allowed.
	Ask(question, def string) (string, error)

	// AskChoice offers numbered options and returns the zero-based index of the one
	// chosen. A def of -1 means there is no default and the question must be
	// answered — which is how the trust question is asked.
	AskChoice(question string, options []string, def int) (int, error)

	// AskYesNo puts a yes/no question with a default.
	AskYesNo(question string, def bool) (bool, error)

	// AskSecret reads a value that must not be echoed. Implementations that cannot
	// suppress echo say so rather than pretending they did.
	AskSecret(question string) (string, error)
}

// prompter implements the whole of IO on top of two primitives: a way to read one
// line, and a way to write one block.
//
// Both the console and the scripted fake embed it, so the questions a test sees are
// rendered by the same code that renders them to a terminal, and an answer a test
// supplies is parsed by the same code that parses a person's typing. A fake that
// re-implemented either would be a fake that agrees with the wizard about
// everything except what the operator experiences.
type prompter struct {
	read  func(prompt string, secret bool) (string, error)
	write func(text string)
}

// Print writes a block of text.
func (p prompter) Print(text string) { p.write(text) }

// Ask puts a free-text question.
func (p prompter) Ask(question, def string) (string, error) {
	line, err := p.read(renderQuestion(question, def), false)
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// AskChoice offers numbered options until one of them is chosen.
func (p prompter) AskChoice(question string, options []string, def int) (int, error) {
	prompt := renderChoice(question, options, def)
	for {
		line, err := p.read(prompt, false)
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" && def >= 0 {
			return def, nil
		}
		n, convErr := strconv.Atoi(line)
		if convErr == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		p.write(fmt.Sprintf("  Please answer with %s.", choiceRange(len(options))))
		// Only the prompt line is repeated. Reprinting the whole question would
		// scroll the options a mistyping reader is trying to look at.
		prompt = renderChoicePrompt(len(options), def)
	}
}

// AskYesNo puts a yes/no question until it gets a yes or a no.
func (p prompter) AskYesNo(question string, def bool) (bool, error) {
	prompt := renderYesNo(question, def)
	for {
		line, err := p.read(prompt, false)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.write("  Please answer yes or no.")
	}
}

// AskSecret reads a value without echoing it.
func (p prompter) AskSecret(question string) (string, error) {
	line, err := p.read(renderQuestion(question, ""), true)
	if err != nil {
		return "", err
	}
	// Only the ends are trimmed. A pasted token can carry a trailing newline or a
	// stray space from a chat client, and neither is part of it; the middle is left
	// exactly as given, because judging the shape of a secret is not this layer's
	// job.
	return strings.TrimSpace(line), nil
}

// renderQuestion renders a free-text prompt. A question mark already ends the
// sentence, so no colon is added after one.
func renderQuestion(question, def string) string {
	q := strings.TrimRight(question, " ")
	if def != "" {
		q += " [" + def + "]"
	}
	if strings.HasSuffix(q, "?") || strings.HasSuffix(q, "]") {
		return q + " "
	}
	return q + ": "
}

// renderYesNo renders a yes/no prompt with the default in capitals, the convention
// every command-line tool has used for thirty years.
func renderYesNo(question string, def bool) string {
	q := strings.TrimRight(question, " ")
	if def {
		return q + " [Y/n] "
	}
	return q + " [y/N] "
}

// renderChoice renders the question, the numbered options and the prompt.
func renderChoice(question string, options []string, def int) string {
	var b strings.Builder
	b.WriteString(question)
	b.WriteString("\n\n")
	for i, opt := range options {
		fmt.Fprintf(&b, "  [%d] %s\n", i+1, opt)
	}
	b.WriteString("\n")
	b.WriteString(renderChoicePrompt(len(options), def))
	return b.String()
}

// renderChoicePrompt is the last line of a choice, on its own, for re-asking.
func renderChoicePrompt(n, def int) string {
	if def >= 0 {
		return fmt.Sprintf("Choose %s [%d]: ", choiceRange(n), def+1)
	}
	return fmt.Sprintf("Choose %s: ", choiceRange(n))
}

// choiceRange names the valid answers to a choice.
func choiceRange(n int) string {
	if n == 2 {
		return "1 or 2"
	}
	return fmt.Sprintf("a number from 1 to %d", n)
}

// ConsoleIO is the IO a person interacts with.
type ConsoleIO struct {
	prompter
	in  *bufio.Reader
	out io.Writer
	// noEcho suppresses terminal echo for the duration of one read. It is nil when
	// input is not a terminal.
	noEcho func(func() error) error
}

// NewConsoleIO returns an IO reading from in and writing to out.
//
// When in is a terminal, AskSecret turns echo off for the duration of the read.
// When it is not — a pipe, a here-document, a test — there is no echo to turn off,
// and the value is read plainly: input that arrives down a pipe was already
// committed to a file or a variable by whoever set the pipe up, and refusing to
// read it would break scripted installs to protect a secret that is not at risk.
func NewConsoleIO(in io.Reader, out io.Writer) *ConsoleIO {
	c := &ConsoleIO{in: bufio.NewReader(in), out: out}
	if f, ok := in.(*os.File); ok {
		c.noEcho = noEchoFor(f)
	}
	c.prompter = prompter{read: c.readLine, write: c.printLine}
	return c
}

// Console returns a ConsoleIO on the process's own standard input and output.
func Console() *ConsoleIO { return NewConsoleIO(os.Stdin, os.Stdout) }

func (c *ConsoleIO) printLine(text string) {
	fmt.Fprintln(c.out, text)
}

func (c *ConsoleIO) readLine(prompt string, secret bool) (string, error) {
	fmt.Fprint(c.out, prompt)

	var (
		line string
		err  error
	)
	readOnce := func() error {
		line, err = c.in.ReadString('\n')
		return err
	}
	if secret && c.noEcho != nil {
		err = c.noEcho(readOnce)
		// The newline the operator typed was swallowed with the echo, so the
		// terminal is still sitting at the end of the prompt.
		fmt.Fprintln(c.out)
	} else {
		err = readOnce()
	}

	switch {
	case errors.Is(err, io.EOF) && strings.TrimSpace(line) != "":
		// A final line with no newline after it is still an answer.
	case errors.Is(err, io.EOF):
		return "", ErrInputClosed
	case err != nil:
		return "", fmt.Errorf("setup: reading input: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ScriptIO is an IO that answers from a list and records everything it was shown.
//
// It exists so the whole wizard, wording included, can be exercised without a
// terminal. Answers are consumed in order, and each is the string a person would
// have typed — parsed by the same code that parses their typing, so a test that
// passes here is a test about what somebody would actually experience.
type ScriptIO struct {
	prompter
	answers []string
	next    int
	log     strings.Builder
}

// NewScriptIO returns a fake IO that will give the supplied answers in order.
func NewScriptIO(answers ...string) *ScriptIO {
	s := &ScriptIO{answers: answers}
	s.prompter = prompter{read: s.readLine, write: s.printLine}
	return s
}

// Transcript returns everything the wizard printed and every prompt it put,
// interleaved in the order they happened and with the answers omitted. It is what
// the operator's terminal would have looked like, minus their own typing.
func (s *ScriptIO) Transcript() string { return s.log.String() }

// Remaining reports how many scripted answers were never used. A test asserting a
// flow ended where it meant to should check it reached zero.
func (s *ScriptIO) Remaining() int { return len(s.answers) - s.next }

func (s *ScriptIO) printLine(text string) {
	s.log.WriteString(text)
	s.log.WriteString("\n")
}

func (s *ScriptIO) readLine(prompt string, secret bool) (string, error) {
	s.log.WriteString(prompt)
	if s.next >= len(s.answers) {
		s.log.WriteString("<no answer>\n")
		return "", ErrInputClosed
	}
	answer := s.answers[s.next]
	s.next++
	if secret {
		// The transcript is compared in golden tests and printed on failure. A
		// scripted secret is not a real one, but a fake that logs values teaches
		// the habit of logging them.
		s.log.WriteString("<not echoed>\n")
	} else {
		s.log.WriteString(answer + "\n")
	}
	return answer, nil
}
