package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// StateFileName is the file inside the data directory that holds enrolment bindings.
const StateFileName = "state.json"

// stateVersion is written into every state file so a future format change can be
// recognised rather than guessed at.
const stateVersion = 1

// Binding is what claiming an invite produces: a Telegram account bound to a member,
// and when it happened.
type Binding struct {
	TelegramID int64     `json:"telegram_id"`
	EnrolledAt time.Time `json:"enrolled_at"`
}

// State is kenward's mutable enrolment record, kept beside the configuration rather
// than inside it.
//
// The split is deliberate. kenward.yaml is declarative, hand-edited and full of an
// operator's comments and formatting; claiming an invite mutates a member's Telegram
// binding. Writing that back into the YAML would mean rewriting a file kenward does not
// own, destroying exactly the parts of it a person put there. So the file stays
// authoritative for what the household *is*, and this holds what has happened since.
//
// It holds what the members themselves have done: claimed an invite, and answered the
// questions their Telegram tutorial put to them. Nothing an operator writes belongs
// here — a member's name, space and tier chain are the file's, and kenward does not
// rewrite the file.
//
// A persona is on this side of that line and it is worth saying why, because it looks
// like configuration. members[].persona is the one section of the schema an operator
// does not fill in: it is written by the member, in Telegram, about their own
// assistant, and the wizard deliberately refuses to answer it on their behalf. So it
// arrives the same way a binding does — from a conversation, at runtime, one answer at
// a time — and it is recorded the same way. MergeState folds it back into the
// configuration, per field, so that everything downstream reads one member list and
// never consults two sources.
type State struct {
	Version int `json:"version"`
	// Bindings is keyed by MemberID: the stable internal id, never the Telegram one,
	// so a member can re-claim from a new account without becoming a new person.
	Bindings map[domain.MemberID]Binding `json:"bindings"`
	// Personas is what each member chose about their own agent, keyed the same way.
	// A member who has never answered has no entry, which is a different fact from
	// an entry full of empty strings and stays distinguishable from it.
	Personas map[domain.MemberID]MemberPersona `json:"personas,omitempty"`
}

// MemberPersona is one member's answers, and the little tutorial progress that has to
// outlive the process to be worth anything.
//
// Persona is the part that is configuration once it has been given. The other two are
// not, and they are here rather than in a file of their own because they are useless
// apart from it: an onboarding that was cut off is identified by having a chat to
// finish in and no explanation sent, and both facts are about the same member's same
// conversation.
type MemberPersona struct {
	Persona PersonaConfig `json:"persona"`
	// TutorialChat is the private chat the member's onboarding ran in, kept so an
	// interrupted one can be finished later without guessing a chat id from a user
	// id. Zero means no onboarding has started.
	TutorialChat int64 `json:"tutorial_chat,omitempty"`
	// Explained records that the memory-model explanation reached this member. It is
	// the one thing kenward owes them rather than asks of them, so a node that died
	// between the greeting and it has to know to send it on the next start.
	Explained bool `json:"explained,omitempty"`
}

// NewState returns an empty state, as a first run has.
func NewState() *State {
	return &State{
		Version:  stateVersion,
		Bindings: make(map[domain.MemberID]Binding),
		Personas: make(map[domain.MemberID]MemberPersona),
	}
}

// MemberPersona returns what a member recorded about their own agent, if anything.
func (s *State) MemberPersona(id domain.MemberID) (MemberPersona, bool) {
	if s == nil {
		return MemberPersona{}, false
	}
	p, ok := s.Personas[id]
	return p, ok
}

// SetMemberPersona records a member's answers, replacing whatever was held.
//
// Replacing rather than merging is what makes an abandoned tutorial cheap: the caller
// holds the whole of what has been answered so far and writes it after every question,
// so there is never a partial record anywhere for a restart to reconcile.
func (s *State) SetMemberPersona(id domain.MemberID, p MemberPersona) {
	if s.Personas == nil {
		s.Personas = make(map[domain.MemberID]MemberPersona)
	}
	s.Version = stateVersion
	s.Personas[id] = p
}

// Binding returns the binding recorded for a member, if there is one.
func (s *State) Binding(id domain.MemberID) (Binding, bool) {
	if s == nil {
		return Binding{}, false
	}
	b, ok := s.Bindings[id]
	return b, ok
}

// Bind records that a member has claimed an invite from a Telegram account. It is what
// the enrolment flow calls once a claim code has been verified.
func (s *State) Bind(id domain.MemberID, telegramID int64, at time.Time) {
	if s.Bindings == nil {
		s.Bindings = make(map[domain.MemberID]Binding)
	}
	s.Version = stateVersion
	s.Bindings[id] = Binding{TelegramID: telegramID, EnrolledAt: at}
}

// Unbind removes a member's binding. It is what revocation calls; the member's space
// key still has to be rotated in lore, which is not something kenward can do for them.
func (s *State) Unbind(id domain.MemberID) {
	delete(s.Bindings, id)
}

// MemberByTelegramID finds which member a Telegram account is bound to, so enrolment
// can refuse a claim from an account that already belongs to someone.
func (s *State) MemberByTelegramID(telegramID int64) (domain.MemberID, bool) {
	if s == nil || telegramID == 0 {
		return "", false
	}
	// Iterated in sorted order so the answer does not depend on map ordering when a
	// state file has somehow ended up with the same account bound twice.
	for _, id := range s.memberIDs() {
		if s.Bindings[id].TelegramID == telegramID {
			return id, true
		}
	}
	return "", false
}

func (s *State) memberIDs() []domain.MemberID {
	ids := make([]domain.MemberID, 0, len(s.Bindings))
	for id := range s.Bindings {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// LoadState reads a state file.
//
// A missing file is not an error: it is what a household that has never enrolled anyone
// looks like, and refusing to start over it would make first run impossible. A file
// that exists but cannot be parsed *is* an error, because guessing at enrolment state
// means guessing at who is allowed to talk to the node.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return NewState(), nil
		}
		return nil, fmt.Errorf("state: reading %s: %w", path, err)
	}

	st := NewState()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("state: parsing %s: %w", path, err)
	}
	if st.Bindings == nil {
		st.Bindings = make(map[domain.MemberID]Binding)
	}
	if st.Personas == nil {
		st.Personas = make(map[domain.MemberID]MemberPersona)
	}
	if st.Version == 0 {
		st.Version = stateVersion
	}
	return st, nil
}

// Save writes the state atomically: a temporary file in the same directory, flushed to
// disk, then renamed over the target.
//
// The atomicity matters more than it looks. A truncated state file is a household whose
// members are all suddenly unenrolled, and the moment it would happen is a power cut
// during the one write per claim.
func (s *State) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: creating %s: %w", dir, err)
	}

	if s.Version == 0 {
		s.Version = stateVersion
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encoding: %w", err)
	}
	data = append(data, '\n')

	// The temporary file is created in the destination directory so the rename is
	// within one filesystem and therefore atomic.
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("state: creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := writeAndSync(tmp, data); err != nil {
		tmp.Close()
		return fmt.Errorf("state: writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("state: replacing %s: %w", path, err)
	}
	syncDir(dir)
	return nil
}

func writeAndSync(f *os.File, data []byte) error {
	// The file holds who is allowed to talk to this household. Other users on the
	// machine have no business reading it, and CreateTemp's default is already 0600 on
	// Unix; this makes it explicit and survives a umask surprise.
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// syncDir flushes the directory entry so the rename itself survives a power loss. It is
// best-effort: Windows cannot open a directory as a file, and a failure here does not
// make the written state any less correct.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// StatePath is where this configuration keeps its state file.
func (c *Config) StatePath() string {
	dir := c.DataDir
	if dir == "" {
		dir = DefaultDataDir()
	}
	return filepath.Join(dir, StateFileName)
}

// RemindersPath is where one unit keeps its reminders. The two arguments name the
// unit between them: group alone is the household group's, member alone is that
// member's own assistant, and both together is their private conversation with
// kenward, which is a third conversation and needs a third file.
//
// One file per unit, never one file keyed by member. That is not tidiness: a member's
// unit is isolated from every other by construction, which is what lets the same code
// run as a goroutine beside its siblings and as a pod on its own, and a shared file
// with a member column would be exactly the shared mutable state that property forbids.
// In isolated mode each pod's data directory is its own volume and holds only this
// file; in simple mode the units share a directory and hold a file each.
func (c *Config) RemindersPath(member domain.MemberID, group bool) string {
	dir := c.DataDir
	if dir == "" {
		dir = DefaultDataDir()
	}
	// Through sanitizeMemberID, which is the function that already decides what a
	// member id looks like as a name on a filesystem — it is how pods are named.
	// A member id is operator-written and otherwise unconstrained, so putting a
	// raw one in a path would let "../../etc/x" out of the data directory; and
	// reusing this one rather than writing a second sanitiser means the
	// uniqueness validateMembers already enforces over pod names holds over these
	// filenames too, for free.
	name := "reminders-group.json"
	switch {
	case !group:
		name = "reminders-member-" + sanitizeMemberID(string(member)) + ".json"
	case member != "":
		name = "reminders-household-" + sanitizeMemberID(string(member)) + ".json"
	}
	return filepath.Join(dir, name)
}

// DefaultDataDir returns the per-OS location for kenward's mutable state, used when
// data_dir is not set.
//
// State, not configuration, so it follows each platform's convention for data a program
// writes about itself: XDG_STATE_HOME on Linux, Application Support on macOS,
// LOCALAPPDATA on Windows. If none of that can be determined — an unusual container
// with no home directory — it falls back to a directory beside the working directory,
// which is wrong in a tidy sense but never silently writes into somebody else's tree.
func DefaultDataDir() string {
	const app = "kenward"

	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, app)
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, "Library", "Application Support", app)
		}
	default:
		if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
			return filepath.Join(dir, app)
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".local", "state", app)
		}
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, app)
	}
	return filepath.Join(".", "."+app)
}

// MergeState folds recorded enrolments into the configuration, so that everything
// downstream sees one answer to "is this member enrolled" and never has to consult two
// sources and reconcile them itself.
//
// State wins over the file, because state is what actually happened. The one case that
// is not a merge is a disagreement: if the YAML names a telegram_id and the state names
// a different one for the same member, somebody has hand-edited the file against a
// binding that already exists. Silently preferring either one would either lock a
// member out or hand their space to an account the operator did not intend, so it is
// reported as a validation problem and kenward does not start.
//
// A binding for a member who is no longer in the file is ignored: deleting a member from
// the configuration is a legitimate way to remove them, and refusing to start over a
// leftover row would strand the whole household. Nothing reads such a binding, because
// every lookup goes through the configured member list.
//
// It returns a *ValidationError listing conflicts, or nil.
func (c *Config) MergeState(st *State) error {
	if st == nil {
		return nil
	}
	p := &problems{}
	for i := range c.Members {
		m := &c.Members[i]
		// Per field, and the member's own answer wins. A field they never answered
		// is an empty string here and leaves whatever the file said, which is what
		// makes an abandoned tutorial equivalent to one that was never started: it
		// cannot blank out a persona somebody wrote by hand, and it cannot half-write
		// one either. There is no disagreement to report, unlike telegram_id below —
		// members[].persona is the member's to write and the file's value is only
		// ever a starting point.
		if mp, ok := st.MemberPersona(domain.MemberID(m.ID)); ok {
			m.Persona = PersonaConfig{
				AgentName: orElse(mp.Persona.AgentName, m.Persona.AgentName),
				Language:  orElse(mp.Persona.Language, m.Persona.Language),
				Tone:      orElse(mp.Persona.Tone, m.Persona.Tone),
				Character: orElse(mp.Persona.Character, m.Persona.Character),
			}
		}
		b, ok := st.Binding(domain.MemberID(m.ID))
		if !ok {
			continue
		}
		switch {
		case m.TelegramID == 0:
			m.TelegramID = b.TelegramID
		case m.TelegramID != b.TelegramID:
			p.addf("members[%d].telegram_id: the file says %d but %s records %d for member %q; one of them is wrong and kenward will not choose for you",
				i, m.TelegramID, StateFileName, b.TelegramID, m.ID)
			continue
		}
		m.EnrolledAt = b.EnrolledAt
	}
	if len(p.list) == 0 {
		return nil
	}
	return &ValidationError{Problems: p.list}
}
