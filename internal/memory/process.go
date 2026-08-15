package memory

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stderrTail is how much of the child's stderr is retained to explain a failed
// start. lore prints one line and exits when its home is not initialised, so a
// small tail is enough.
const stderrTail = 8 << 10

// child is a running `lore mcp` subprocess.
//
// The process is owned entirely here: this is the only place that starts it, the
// only place that waits on it, and the only place that kills it. Both pipes are
// created explicitly rather than through exec's StdoutPipe so that waiting on the
// process cannot close the read end from under an in-flight response.
type child struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *ringWriter

	exited  chan struct{}
	waitErr error

	stopOnce sync.Once
}

// startChild launches the subprocess and returns it with its stdio wired up.
func startChild(cfg Config) (*child, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("memory: stdin pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, fmt.Errorf("memory: stdout pipe: %w", err)
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.childEnv()
	cmd.Stdin = inR
	cmd.Stdout = outW
	stderr := newRingWriter(stderrTail)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("memory: starting %s: %w", cfg.Command, err)
	}
	// The parent keeps only the ends it uses; the child holds the others, so
	// closing inW gives the child EOF and the child exiting gives us EOF.
	inR.Close()
	outW.Close()

	c := &child{cmd: cmd, stdin: inW, stdout: outR, stderr: stderr, exited: make(chan struct{})}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.exited)
	}()
	return c, nil
}

// alive reports whether the process is still running.
func (c *child) alive() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

// stop shuts the process down the way the MCP stdio transport prescribes: close
// its input so it sees EOF, give it grace to exit, then kill it. It always waits
// for the process to be reaped, so it never leaves an orphan behind.
func (c *child) stop(grace time.Duration) {
	c.stopOnce.Do(func() {
		_ = c.stdin.Close()
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-c.exited:
		case <-t.C:
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-c.exited
		}
		_ = c.stdout.Close()
	})
}

// stderrSuffix renders the child's stderr for inclusion in a start-up error.
func (c *child) stderrSuffix() string {
	s := c.stderr.String()
	if s == "" {
		return ""
	}
	return "; stderr: " + s
}

// session is one connected `lore mcp` subprocess plus its MCP client session.
type session struct {
	proc *child
	mcp  *mcp.ClientSession
	// done is closed when the MCP connection ends, whether because the
	// subprocess exited, because its stdout closed, or because it was closed
	// from here.
	done chan struct{}
}

// alive reports whether both the subprocess and the MCP connection are usable.
func (s *session) alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return s.proc.alive()
	}
}

// waitDead reports whether the session has ended, waiting up to d for the news.
// A call that fails because the subprocess is going away often returns just
// before the connection is observed to be closed.
func (s *session) waitDead(d time.Duration) bool {
	if !s.alive() {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return true
	case <-s.proc.exited:
		return true
	case <-t.C:
		return false
	}
}

// dial starts a subprocess and completes the MCP handshake with it.
func dial(ctx context.Context, cfg Config) (*session, error) {
	proc, err := startChild(cfg)
	if err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, cfg.StartTimeout)
	defer cancel()

	cl := mcp.NewClient(&mcp.Implementation{Name: "kenward", Version: "0"}, nil)
	transport := &mcp.IOTransport{
		// The transport must not own either pipe: child.stop does.
		Reader: io.NopCloser(proc.stdout),
		Writer: nopWriteCloser{proc.stdin},
	}
	cs, err := cl.Connect(connectCtx, transport, nil)
	if err != nil {
		suffix := ""
		if !proc.alive() {
			suffix = proc.stderrSuffix()
		}
		proc.stop(cfg.ShutdownGrace)
		if suffix == "" {
			suffix = proc.stderrSuffix()
		}
		return nil, fmt.Errorf("memory: lore MCP handshake failed: %w%s", err, suffix)
	}
	s := &session{proc: proc, mcp: cs, done: make(chan struct{})}
	// One supervisor goroutine per session; it returns when the connection ends,
	// which close guarantees.
	go func() {
		_ = cs.Wait()
		close(s.done)
	}()
	return s, nil
}

// close ends the MCP session and then the subprocess. It is safe to call more
// than once and always waits for the subprocess to be reaped.
func (s *session) close(grace time.Duration) {
	_ = s.mcp.Close()
	s.proc.stop(grace)
	<-s.done
}

// nopWriteCloser adapts a writer whose lifetime is managed elsewhere.
type nopWriteCloser struct{ io.Writer }

// Close does nothing; the pipe is closed by child.stop.
func (nopWriteCloser) Close() error { return nil }

// ringWriter keeps the last n bytes written to it. It is safe for concurrent use
// because os/exec writes to it from its own goroutine.
type ringWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newRingWriter(max int) *ringWriter { return &ringWriter{max: max} }

// Write implements io.Writer, discarding all but the trailing max bytes.
func (w *ringWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// String returns the retained tail.
func (w *ringWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
