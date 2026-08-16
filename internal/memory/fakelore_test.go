package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file is a scripted stand-in for `lore mcp`. It is a real subprocess
// speaking real MCP over real pipes — the test binary re-executes itself — so the
// client's process lifecycle, timeouts and restart behaviour are exercised end to
// end without any lore binary being installed.

const (
	fakeEnvOn     = "KENWARD_FAKE_LORE"
	fakeEnvScript = "KENWARD_FAKE_LORE_SCRIPT"
	fakeEnvLog    = "KENWARD_FAKE_LORE_LOG"
)

// fakeReply is one scripted answer.
type fakeReply struct {
	// Match, when set, restricts this reply to calls whose encoded arguments
	// contain it. It is how a concurrent fan-out across spaces gets a
	// deterministic answer per space.
	Match   string `json:"match,omitempty"`
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
	DelayMS int    `json:"delay_ms,omitempty"`
}

// fakeScript drives the fake server's behaviour.
type fakeScript struct {
	// ExitBeforeServe makes the process print Stderr and exit without ever
	// completing the MCP handshake, the way `lore mcp` does when LORE_HOME has
	// not been initialised.
	ExitBeforeServe bool `json:"exit_before_serve,omitempty"`
	// Stderr is written to standard error at start-up.
	Stderr string `json:"stderr,omitempty"`
	// ExitCode is used with ExitBeforeServe.
	ExitCode int `json:"exit_code,omitempty"`
	// Replies are consumed in order per tool; the last one repeats.
	Replies map[string][]fakeReply `json:"replies,omitempty"`
	// DieOnCall is the 1-based index of the call, counted across all tools and
	// across restarts, on which the process exits without answering. Counting
	// across restarts is what lets a test assert that the client came back.
	DieOnCall int `json:"die_on_call,omitempty"`
	// HangOnCall is the 1-based index of the call that never returns.
	HangOnCall int `json:"hang_on_call,omitempty"`
}

// fakeCall is one line of the call log the fake writes for the test to inspect.
type fakeCall struct {
	PID  int             `json:"pid"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// TestHelperLoreServer is the entry point re-executed as the fake lore server. It
// is inert in a normal test run.
func TestHelperLoreServer(t *testing.T) {
	if os.Getenv(fakeEnvOn) != "1" {
		t.Skip("helper process for the fake lore server")
	}
	runFakeLore()
}

// runFakeLore serves MCP over stdio and never returns.
func runFakeLore() {
	var script fakeScript
	if raw := os.Getenv(fakeEnvScript); raw != "" {
		if err := json.Unmarshal([]byte(raw), &script); err != nil {
			fmt.Fprintf(os.Stderr, "fake lore: bad script: %v\n", err)
			os.Exit(2)
		}
	}
	if script.Stderr != "" {
		fmt.Fprint(os.Stderr, script.Stderr)
	}
	if script.ExitBeforeServe {
		code := script.ExitCode
		if code == 0 {
			code = 1
		}
		os.Exit(code)
	}

	logPath := os.Getenv(fakeEnvLog)
	var (
		mu    sync.Mutex
		local int
		next  = map[string]int{}
	)

	srv := mcp.NewServer(&mcp.Implementation{Name: "lore", Version: "0.2.0"}, nil)
	for _, name := range []string{toolSearch, toolGet, toolPut, toolSpaces, toolShare, toolDelete} {
		srv.AddTool(
			&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				tool := req.Params.Name
				args := string(req.Params.Arguments)

				mu.Lock()
				local++
				n := local
				if logPath != "" {
					appendCall(logPath, fakeCall{PID: os.Getpid(), Tool: tool, Args: req.Params.Arguments})
					// Counting from the shared log makes the index global
					// across restarts.
					if c := countLines(logPath); c > 0 {
						n = c
					}
				}
				// Pick the reply bucket that matches these arguments, then the
				// next unused reply in it.
				mkey := ""
				for _, r := range script.Replies[tool] {
					if r.Match != "" && strings.Contains(args, r.Match) {
						mkey = r.Match
						break
					}
				}
				var bucket []fakeReply
				for _, r := range script.Replies[tool] {
					if r.Match == mkey {
						bucket = append(bucket, r)
					}
				}
				key := tool + "\x00" + mkey
				i := next[key]
				if i >= len(bucket) {
					i = len(bucket) - 1
				}
				next[key] = i + 1
				mu.Unlock()

				if script.DieOnCall > 0 && n == script.DieOnCall {
					os.Exit(3)
				}
				if script.HangOnCall > 0 && n == script.HangOnCall {
					<-make(chan struct{})
				}
				if len(bucket) == 0 {
					return textResult("no scripted reply for "+tool, true), nil
				}
				r := bucket[i]
				if r.DelayMS > 0 {
					time.Sleep(time.Duration(r.DelayMS) * time.Millisecond)
				}
				return textResult(r.Text, r.IsError), nil
			})
	}
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
	os.Exit(0)
}

// textResult builds the single-text-block result shape every lore tool uses.
func textResult(text string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: isErr,
	}
}

// appendCall records one call for the parent test to read back.
func appendCall(path string, c fakeCall) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

// countLines returns the number of lines currently in the call log.
func countLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return bytes.Count(b, []byte{'\n'})
}

// fake is a running Client wired to the scripted server, plus access to its call
// log.
type fake struct {
	*Client
	logPath string
}

// newFake starts a Client backed by the scripted fake server. The subprocess is
// lazy, so nothing runs until the first call.
func newFake(t *testing.T, script fakeScript, tweak func(*Config)) *fake {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	raw, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshalling the script: %v", err)
	}
	logPath := t.TempDir() + string(os.PathSeparator) + "calls.jsonl"

	cfg := Config{
		Command: exe,
		Args:    []string{"-test.run=^TestHelperLoreServer$"},
		// LoreHome is what isolates one lore instance from another; the fake
		// does not read it, but every test proves it is exported.
		LoreHome: t.TempDir(),
		Env: []string{
			fakeEnvOn + "=1",
			fakeEnvScript + "=" + string(raw),
			fakeEnvLog + "=" + logPath,
		},
		CallTimeout:   10 * time.Second,
		StartTimeout:  30 * time.Second,
		ShutdownGrace: 2 * time.Second,
		BusyRetries:   3,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &fake{Client: c, logPath: logPath}
}

// calls reads the fake's call log.
func (f *fake) calls(t *testing.T) []fakeCall {
	t.Helper()
	b, err := os.ReadFile(f.logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the call log: %v", err)
	}
	var out []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var c fakeCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("bad call log line %q: %v", line, err)
		}
		out = append(out, c)
	}
	return out
}

// argsOf decodes the arguments of one logged call.
func argsOf(t *testing.T, c fakeCall) map[string]any {
	t.Helper()
	m := map[string]any{}
	if len(c.Args) == 0 {
		return m
	}
	if err := json.Unmarshal(c.Args, &m); err != nil {
		t.Fatalf("bad logged arguments %s: %v", c.Args, err)
	}
	return m
}

// callsTo filters the log to one tool.
func callsTo(calls []fakeCall, tool string) []fakeCall {
	var out []fakeCall
	for _, c := range calls {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

// pids returns the distinct subprocess pids seen, in first-seen order.
func pids(calls []fakeCall) []int {
	var out []int
	seen := map[int]bool{}
	for _, c := range calls {
		if !seen[c.PID] {
			seen[c.PID] = true
			out = append(out, c.PID)
		}
	}
	return out
}
