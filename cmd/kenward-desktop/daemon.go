package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// daemonState is what the icon is drawing.
type daemonState int

const (
	// stateStopped is before the first start and during the drain on quit. It is
	// also what a household sees for the second or two between processes.
	stateStopped daemonState = iota
	// stateRunning is a live child that has been up long enough to be believed.
	stateRunning
	// stateFailed is a child that would not start, or one that exited on its own.
	// It is not terminal: the supervisor keeps trying, and the icon says so.
	stateFailed
)

// settled is how long a child has to survive before its start counts as a success
// and the backoff resets.
//
// `kenward run` refuses to start on a bad configuration, an unreadable secret or a
// lore that does not answer, and it does all of that within a second or two. A child
// that is still alive after this long has got past every one of those refusals and is
// actually serving; one that has not is in the failure loop this bounds.
const settled = 30 * time.Second

// backoffMax caps the wait between restarts.
//
// There is no give-up. A household's assistant that stopped retrying would need
// somebody to notice a grey icon and know what to do about it, and the usual cause of
// a restart loop — an endpoint down, a token rotated, a machine asleep — fixes itself
// when the cause does. So the supervisor keeps trying at this interval forever, and
// the icon stays red until it works.
const backoffMax = 30 * time.Second

// drainTimeout is how long quit waits for the household to finish in flight.
//
// It matches internal/supervisor.DefaultDrainTimeout, deliberately and by copy: this
// binary must not import the daemon's internals, because doing so would pull the
// supervisor, the transport and the assistant into a cgo build. The number being the
// same is what matters; if it drifts, quitting kills a turn that the daemon was still
// finishing.
const drainTimeout = 3 * time.Minute

// daemon supervises exactly one `kenward run`.
type daemon struct {
	exe        string
	configPath string

	// onChange is called on every state transition, from the supervisor goroutine.
	onChange func(daemonState)

	mu     sync.Mutex
	state  daemonState
	detail string
	cmd    *exec.Cmd

	stopOnce sync.Once
	stopping chan struct{}
	done     chan struct{}
}

func newDaemon(exe, configPath string) *daemon {
	return &daemon{
		exe:        exe,
		configPath: configPath,
		state:      stateStopped,
		detail:     "Daemon: starting",
		stopping:   make(chan struct{}),
		done:       make(chan struct{}),
	}
}

func (d *daemon) snapshot() (daemonState, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state, d.detail
}

func (d *daemon) set(st daemonState, format string, args ...any) {
	d.mu.Lock()
	d.state, d.detail = st, fmt.Sprintf(format, args...)
	cb := d.onChange
	d.mu.Unlock()
	if cb != nil {
		cb(st)
	}
}

func (d *daemon) start() { go d.supervise() }

// supervise is the whole restart loop.
func (d *daemon) supervise() {
	defer close(d.done)
	backoff := time.Second
	for {
		// Checked here as well as after the wait, so that a quit arriving while
		// this loop is between children does not start one more.
		select {
		case <-d.stopping:
			return
		default:
		}

		started := time.Now()
		err := d.runOnce()
		select {
		case <-d.stopping:
			return
		default:
		}

		lived := time.Since(started)
		if lived >= settled {
			// It worked for a while and then stopped. Whatever went wrong is
			// unlikely to be the thing that fails instantly on the next start, so
			// come straight back rather than making the household wait out a
			// backoff it did not earn. This is also the path an auto-update takes:
			// `kenward run` exits non-zero on purpose after swapping its own
			// binary, expecting a service manager to bring it back, and here that
			// service manager is this loop.
			backoff = time.Second
			d.set(stateFailed, "Daemon: exited after %s — restarting", lived.Round(time.Second))
		} else {
			d.set(stateFailed, "Daemon: %v — retrying in %s", errText(err), backoff)
		}

		select {
		case <-time.After(backoff):
		case <-d.stopping:
			return
		}
		if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// runOnce starts one child and blocks until it exits.
func (d *daemon) runOnce() error {
	cmd := exec.Command(d.exe, "run", "--config", d.configPath)
	// The daemon's own logging is structured and goes to stderr. Passing it
	// through means `kenward-desktop` run from a terminal shows exactly what the
	// service unit would; in a bundle nobody sees it, which is what the platform
	// log is for and not something this wrapper should reinvent.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	newProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return err
	}
	d.mu.Lock()
	d.cmd = cmd
	d.mu.Unlock()
	d.set(stateRunning, "Daemon: running (pid %d)", cmd.Process.Pid)

	// Asking this child to drain is this child's own goroutine's job, rather than
	// something stop reaches in and does. Otherwise there is a window — stop reads
	// no child because the loop is between attempts, then the loop starts one — in
	// which a child is spawned that nobody ever signals, and quit hangs until the
	// drain timeout kills a daemon that was never asked to stop.
	exited := make(chan struct{})
	defer close(exited)
	go func() {
		select {
		case <-d.stopping:
			if err := interrupt(cmd.Process); err != nil {
				log.Printf("asking pid %d to drain: %v", cmd.Process.Pid, err)
			}
		case <-exited:
		}
	}()

	err := cmd.Wait()
	d.mu.Lock()
	d.cmd = nil
	d.mu.Unlock()
	return err
}

// stop asks the child to drain, waits for it, and only then gives up and kills it.
//
// The graceful signal is the point. `kenward run` turns SIGINT/SIGTERM into a drain:
// intake stops, in-flight turns finish, every session key is zeroed. Killing it
// instead cuts a member's answer mid-sentence.
func (d *daemon) stop() {
	d.stopOnce.Do(func() { close(d.stopping) })
	select {
	case <-d.done:
	case <-time.After(drainTimeout):
		d.mu.Lock()
		cmd := d.cmd
		d.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			log.Printf("pid %d did not drain in %s; killing it", cmd.Process.Pid, drainTimeout)
			_ = cmd.Process.Kill()
		}
		<-d.done
	}
	d.set(stateStopped, "Daemon: stopped")
}

func errText(err error) string {
	if err == nil {
		return "exited"
	}
	return err.Error()
}
