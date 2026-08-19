package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/session"
	"github.com/BlueHeisenberg/kenward/internal/setup"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// sessionsFileName is where wrapped member keys are persisted, under the data
// directory.
//
// It is internal/supervisor's constant rather than a second copy of the literal.
// `run` has to build the session manager itself in order to unlock it at startup,
// and a manager over a different file from the one the supervisor would have used
// is a manager the units never see — so the two cannot be allowed to drift, and the
// only way to be sure of that is for there to be one of them.
const sessionsFileName = supervisor.SessionsFileName

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

// readPassphrase finds the passphrase this process unwraps keys with.
//
// ref, when non-nil, is the passphrase of the one member this process serves —
// `members[].passphrase_env` / `_file`, or the systemd credential `passphrase.<id>`.
// It comes first because in isolated mode it is the only right answer: each member's
// key is wrapped under their own passphrase, so a pod that fell back to a node-wide
// one would unwrap nothing, or worse, would provision a first key under a passphrase
// every other pod also holds. Its absence is not a failure here — the node mechanisms
// below still apply to a pod started by hand — but in a supervisor-started pod it is
// what arrives, mirrored from whichever source the household configuration names.
//
// The rest of the precedence is deliberate too. A systemd credential is a file only
// this service can read; an environment variable is visible in /proc and is inherited
// by the `lore` subprocess; a terminal prompt needs somebody standing there. The
// mechanism with the smallest blast radius is tried first.
//
// It never travels over Telegram, in either direction and in either mode. Asking for
// it in a chat message would hand it to Telegram's servers and leave it in the
// member's own message history, which is worse than the problem it would solve — see
// internal/privacy's isolated-mode statement, which says so to the member.
func readPassphrase(e *env, ref *config.SecretRef) (*passphrase, error) {
	if ref != nil {
		sec, err := e.secrets().Resolve(*ref)
		var se *config.SecretError
		switch {
		case err == nil:
			return &passphrase{b: []byte(sec.Value()), source: sec.Source()}, nil
		case errors.As(err, &se) && se.NotFound:
			// Nothing named it and no credential was there. The node mechanisms
			// below are still legitimate for a pod somebody started by hand.
		default:
			return nil, err
		}
	}
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
		console := setup.NewConsoleIO(e.stdin, e.stderr)
		v, err := console.AskSecret("Passphrase for this node's member keys")
		switch {
		case errors.Is(err, setup.ErrInputClosed), errors.Is(err, io.EOF):
			// Nobody was there after all — see isTerminal for why this is
			// reachable. Fall through to the refusal, which explains the three
			// ways to supply a passphrase, rather than reporting the wizard's
			// "input ended" at somebody who never opted into a prompt.
		case err != nil:
			return nil, err
		case v != "":
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

// isTerminal reports whether somebody might be there to be asked.
//
// A pipe, a file and a socket are definitely not a terminal, and prompting into one
// would block a service nobody is watching — a worse failure than refusing to start.
//
// It asks internal/setup, which performs the per-platform terminal ioctl it needs
// anyway in order to suppress echo. The cheap test this used to make — a character
// device — was a necessary condition and not a sufficient one, and the gap was not
// theoretical: `docker run` without -i gives the process /dev/null on standard input,
// /dev/null is a character device, so every non-interactive container wrote
// "Passphrase for this node's member keys: " to its log and then refused to start on
// the end-of-input that was always going to come. The prompt is now offered only where
// somebody could answer it, which is also the only place it could be typed unechoed.
//
// End-of-input is still handled by the caller. A person at a real terminal may press
// Ctrl-D, and that is nobody being there after all.
func isTerminal(r any) bool {
	f, ok := r.(*os.File)
	return ok && setup.IsTerminal(f)
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
		"In isolated mode each member's key is wrapped under that member's own passphrase, so\n" +
		"it is named per member in kenward.yaml, beside their bot token:\n\n" +
		"  members:\n" +
		"    - id: david\n" +
		"      bot_token_env: KENWARD_BOT_TOKEN_DAVID\n" +
		"      passphrase_env: KENWARD_PASSPHRASE_DAVID   # or passphrase_file: /run/secrets/…\n\n" +
		"The host supervisor reads that on the host and provisions it into david's pod alone,\n" +
		"in the same form; a compose deployment sets the same variable on david's service and\n" +
		"nowhere else. With neither field, the systemd credential passphrase.david is used.\n\n" +
		"In simple mode one node passphrase wraps every member's key. Supply it in one of\n" +
		"these, in order of preference:\n" +
		"  1. name it in kenward.yaml, which is the only one of these whose absence is\n" +
		"     reported at load with the variable or path named:\n\n" +
		"       session:\n" +
		"         passphrase_file: /run/secrets/kenward-passphrase   # or passphrase_env: NAME\n\n" +
		"  2. a systemd credential: LoadCredential=" + credentialName + ":/path/to/file in the\n" +
		"     unit, which kenward reads from $" + envCredentialsDirectory + "/" + credentialName + "\n" +
		"  3. the " + envPassphrase + " environment variable\n" +
		"  4. type it at the prompt, if you are starting kenward by hand at a terminal\n\n" +
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
//
// It runs at startup over everyone this process serves, and again — over one member
// — when somebody claims their invite while the node is running. The two are the
// same work, so they are the same function: a second implementation of "provision if
// missing, then unlock" is a second one to get wrong.
func unlockSessions(ctx context.Context, mgr *session.Manager, store session.Store, members []domain.Member, secret string) (unlockReport, error) {
	var rep unlockReport
	for _, m := range members {
		if !m.Enrolled() {
			// Nothing was provisioned for them and nothing needs to be: a member
			// who has not claimed their invite has no unit either.
			continue
		}
		if m.SharedOnly {
			// A key wraps a member's own memory, and this member has none. Their
			// conversations read and write the household's shared space, which is
			// the node's own and is not wrapped per member. Provisioning one here
			// would leave a wrapped key in the store for a space that does not
			// exist — which `kenward sessions` and `doctor` would then both report
			// as this member's private memory, in a household that has told them
			// they have none.
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
		// A shared_only member has no private memory, so no key: an absent one is
		// the design working. Reporting it would be doctor warning, at every run and
		// for ever, that a member cannot read something they were told they do not
		// have.
		if m.Enrolled() && !m.SharedOnly && !have[m.ID] {
			out.MissingKey = append(out.MissingKey, m.ID)
		}
	}
	sort.Slice(out.MissingKey, func(i, j int) bool { return out.MissingKey[i] < out.MissingKey[j] })
	return out
}
