package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// sessionsFileName is where wrapped member keys are persisted, under the data
// directory.
//
// TODO(cmd/kenward): this string is also written down, unexported, inside
// internal/supervisor. `run` has to build the session manager itself in order to
// unlock it at startup, and a manager over a different file from the one the
// supervisor would have used is a manager the units never see. Exporting the name
// from internal/supervisor would remove the chance of the two drifting apart.
const sessionsFileName = "sessions.json"

// Where a node's passphrase comes from, in precedence order.
const (
	// envCredentialsDirectory is systemd's. `LoadCredential=` or
	// `SetCredentialEncrypted=` puts the value in a file in this directory,
	// readable only by the service, never in the process environment where it
	// would be visible in /proc and inherited by the `lore` subprocess.
	envCredentialsDirectory = "CREDENTIALS_DIRECTORY"
	// credentialName is the file inside that directory.
	credentialName = "kenward-passphrase"
	// envPassphrase is the plainer mechanism, for compose and for anyone not on
	// systemd. It is second because an environment variable is inherited by every
	// child process kenward starts.
	envPassphrase = "KENWARD_PASSPHRASE"
)

// passphrase holds a node or member passphrase.
//
// It is a byte slice rather than a string so that the copy this process controls can
// be overwritten once it has been used. The overwrite is not total: session.Manager
// takes a string, and converting allocates a copy that only the garbage collector
// can reclaim. Saying that plainly is better than a zero() call that implies more
// than it does — what this buys is that the buffer read from the credential file
// does not sit in memory for the life of the process.
type passphrase struct {
	b []byte
	// source names where it came from, for an error message that tells somebody
	// which mechanism to fix. It never carries any part of the value.
	source string
}

func (p *passphrase) empty() bool { return p == nil || len(p.b) == 0 }

func (p *passphrase) reveal() string { return string(p.b) }

// zero overwrites the buffer this process read.
func (p *passphrase) zero() {
	if p == nil {
		return
	}
	for i := range p.b {
		p.b[i] = 0
	}
	p.b = nil
}

// errNoPassphrase is what `run` refuses to start with.
var errNoPassphrase = errors.New("no session passphrase available")

// readPassphrase finds the node's passphrase.
//
// The precedence is deliberate. A systemd credential is a file only this service can
// read; an environment variable is visible in /proc and is inherited by the `lore`
// subprocess; a terminal prompt needs somebody standing there. The mechanism with the
// smallest blast radius is tried first.
//
// It never travels over Telegram, in either direction and in either mode. Asking for
// it in a chat message would hand it to Telegram's servers and leave it in the
// member's own message history, which is worse than the problem it would solve — see
// internal/privacy's isolated-mode statement, which says so to the member.
func readPassphrase(e *env) (*passphrase, error) {
	if dir, ok := e.env()(envCredentialsDirectory); ok && dir != "" {
		path := filepath.Join(dir, credentialName)
		b, err := os.ReadFile(path)
		switch {
		case err == nil && len(trimNewline(b)) > 0:
			return &passphrase{b: trimNewline(b), source: path}, nil
		case err != nil && !os.IsNotExist(err):
			return nil, fmt.Errorf("reading the passphrase credential at %s: %w", path, err)
		}
	}
	if v, ok := e.env()(envPassphrase); ok && v != "" {
		return &passphrase{b: []byte(v), source: envPassphrase}, nil
	}
	if isTerminal(e.stdin) {
		// internal/setup already suppresses echo on every platform kenward runs
		// on; a second implementation here is a second one to get wrong.
		io := setup.NewConsoleIO(e.stdin, e.stderr)
		v, err := io.AskSecret("Passphrase for this node's member keys")
		if err != nil {
			return nil, err
		}
		if v != "" {
			return &passphrase{b: []byte(v), source: "the terminal"}, nil
		}
	}
	return nil, errNoPassphrase
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// isTerminal reports whether somebody is there to be asked.
//
// A character device on standard input is a terminal; a pipe, a file, a socket and a
// container with no tty are not. Prompting into any of those blocks a service that
// nobody is watching, which is a worse failure than refusing to start.
func isTerminal(r any) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// noPassphraseHelp is the refusal.
//
// This refusal is the whole point of the check. session.Manager holds no unwrapped
// key until somebody supplies a passphrase, so a node that starts without one answers
// every private message with the locked notice, indefinitely, while its group chat
// works perfectly. That failure gets blamed on the model and investigated for a week.
// Refusing to start gets fixed in five minutes.
func noPassphraseHelp() string {
	return "no session passphrase available, so no member's key can be unwrapped.\n\n" +
		"kenward will not start without one. A node that started anyway would answer every\n" +
		"private message with the locked notice — forever, and only in private chats, so the\n" +
		"household group would look fine and nobody would know where to look.\n\n" +
		"Supply it in one of these, in order of preference:\n" +
		"  1. a systemd credential: LoadCredential=" + credentialName + ":/path/to/file in the\n" +
		"     unit, which kenward reads from $" + envCredentialsDirectory + "/" + credentialName + "\n" +
		"  2. the " + envPassphrase + " environment variable\n" +
		"  3. type it at the prompt, if you are starting kenward by hand at a terminal\n\n" +
		"It is never asked for over Telegram, in either direction: sending it in a chat\n" +
		"message would hand it to Telegram's servers and leave it in the message history."
}

// unlockReport is what happened at startup.
type unlockReport struct {
	Unlocked    []domain.MemberID
	Provisioned []domain.MemberID
}

// unlockSessions provisions the members who have no key yet and unlocks everybody
// else, so that the node can actually answer a private message.
//
// Provision is a first-run path: it generates the member's space key, wraps it and
// persists it, and deliberately leaves nothing unlocked, so each provision is
// followed by an unlock. In simple mode Provision also verifies the passphrase
// against a member already provisioned, which is what makes "one node passphrase
// wraps every member's key" enforced rather than assumed — a typo on the second start
// is refused instead of quietly creating a household with two passphrases.
func unlockSessions(ctx context.Context, mgr *session.Manager, store session.Store, members []domain.Member, pass *passphrase) (unlockReport, error) {
	var rep unlockReport
	secret := pass.reveal()
	for _, m := range members {
		if !m.Enrolled() {
			// Nothing was provisioned for them and nothing needs to be: a member
			// who has not claimed their invite has no unit either.
			continue
		}
		_, err := store.Load(ctx, m.ID)
		switch {
		case errors.Is(err, session.ErrUnknownMember):
			if err := mgr.Provision(ctx, m.ID, secret); err != nil {
				return rep, fmt.Errorf("provisioning a key for %s: %w", m.ID, err)
			}
			rep.Provisioned = append(rep.Provisioned, m.ID)
		case err != nil:
			return rep, fmt.Errorf("reading the wrapped key for %s: %w", m.ID, err)
		}
		if err := mgr.Unlock(ctx, m.ID, secret); err != nil {
			return rep, fmt.Errorf("unlocking %s: %w", m.ID, err)
		}
		rep.Unlocked = append(rep.Unlocked, m.ID)
	}
	return rep, nil
}

// sessionStorePath is where this configuration keeps its wrapped keys.
func sessionStorePath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, sessionsFileName)
}

// sessionMode maps the deployment mode onto session custody. In simple mode one
// operator-held passphrase wraps every member's key; in isolated mode each member's
// key is wrapped under their own, which is the mechanism the mode's privacy claim
// rests on.
func sessionMode(m config.Mode) session.Mode {
	if m == config.ModeIsolated {
		return session.ModeIsolated
	}
	return session.ModeSimple
}

// sessionsResult is what doctor found about key custody.
type sessionsResult struct {
	// Custody is session.CustodyReport's own words. doctor does not paraphrase it.
	Custody string
	// Provisioned are the members holding a wrapped key.
	Provisioned []domain.MemberID
	// MissingKey are members who are enrolled but have no wrapped key. Every private
	// message to them is refused as locked until a node starts with a passphrase.
	MissingKey []domain.MemberID
	Err        error
}

// probeSessions inspects the wrapped-key store without unlocking anything.
//
// It needs no passphrase and derives no key: it reads which members have a record
// and asks the manager for its custody report. What it cannot report is whether a
// key is unwrapped right now — that lives in the running node's memory and this is a
// different process — so it reports the thing that is knowable and useful instead,
// which is whether the node has anything it could unlock at all.
func probeSessions(ctx context.Context, cfg *config.Config) sessionsResult {
	store := session.NewFileStore(sessionStorePath(cfg))
	mgr, err := session.NewManager(sessionMode(cfg.Mode), store)
	if err != nil {
		return sessionsResult{Err: err}
	}
	defer mgr.Close()

	report, err := mgr.Custody(ctx)
	if err != nil {
		return sessionsResult{Err: err}
	}
	out := sessionsResult{Custody: report.String(), Provisioned: report.Members}

	have := make(map[domain.MemberID]bool, len(report.Members))
	for _, id := range report.Members {
		have[id] = true
	}
	for _, m := range cfg.DomainMembers() {
		if m.Enrolled() && !have[m.ID] {
			out.MissingKey = append(out.MissingKey, m.ID)
		}
	}
	sort.Slice(out.MissingKey, func(i, j int) bool { return out.MissingKey[i] < out.MissingKey[j] })
	return out
}
