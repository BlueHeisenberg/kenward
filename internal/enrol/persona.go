package enrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Register values. Empty is not one of them: it means the member never answered and
// inherits the household's, which is a different fact from choosing the flat
// register on purpose and has to stay distinguishable from it.
const (
	// RegisterFlat is kenward's default voice: short, direct, no small talk.
	RegisterFlat = "flat"
	// RegisterWarm is friendly and still brief.
	RegisterWarm = "warm"
	// RegisterPlayful allows humour.
	RegisterPlayful = "playful"
)

// Limits on what a member may type at the tutorial. Both are generous for the thing
// being asked for and small enough that a paste accident cannot become a system
// prompt.
const (
	// MaxAgentNameLen is the longest name a member may give their agent.
	MaxAgentNameLen = 40
	// MaxCharacterLen is the longest character description a member may write.
	MaxCharacterLen = 300
)

// Persona is what one member chose about their own agent in the Telegram tutorial.
//
// Every field's zero value means "not answered, use the household's", which is what
// makes skipping a question and abandoning the tutorial the same thing downstream:
// there is no half-configured state to reason about, only fields that were set and
// fields that were not.
//
// ponytail: these four fields are internal/config's to own, and the store below is a
// placeholder for wherever that package decides personas live. This package writes
// against them so the tutorial could be built and run for real before that landed;
// the merge should be a rename of these fields and a deletion of PersonaFile, not a
// rewrite of the tutorial.
type Persona struct {
	// Language is a tag from this package's list, or a free-text language the member
	// named that the tutorial itself is not written in. Empty inherits.
	Language string `json:"language,omitempty"`
	// AgentName is what this member's agent calls itself. Meaningful under "one
	// agent each" only; empty means kenward.
	AgentName string `json:"agent_name,omitempty"`
	// Register is one of the Register constants. Empty inherits.
	Register string `json:"register,omitempty"`
	// Character is the member's own words about how their agent should be. Empty is
	// the default and is the common case.
	Character string `json:"character,omitempty"`

	// ChatID is the private chat the tutorial ran in, kept so an interrupted one can
	// be finished later without guessing at a chat id from a user id.
	ChatID int64 `json:"chat_id,omitempty"`
	// Explained records that the memory-model explanation reached this member.
	//
	// It is not a persona setting; it is the only piece of tutorial progress worth
	// persisting. Everything else is committed as it is answered, so an interrupted
	// tutorial degrades to defaults on its own — but the explanation is the part
	// kenward owes the member rather than the part it is asking them for, and a node
	// that restarted between the greeting and it would otherwise never send it.
	Explained bool `json:"explained,omitempty"`
}

// PersonaStore records what a member chose, one answer at a time.
//
// The tutorial writes through it after every question rather than at the end, which
// is what makes abandonment cheap: the answers already given are already saved, and
// there is no in-progress record anywhere for a restart to leave stale.
type PersonaStore interface {
	// SetPersona replaces the persona held for a member.
	SetPersona(ctx context.Context, id domain.MemberID, p Persona) error
	// Personas returns every persona held.
	Personas(ctx context.Context) (map[domain.MemberID]Persona, error)
}

// personaFileVersion is written into the file so a format change is detected rather
// than mis-parsed.
const personaFileVersion = 1

type personaFile struct {
	Version  int                         `json:"version"`
	Personas map[domain.MemberID]Persona `json:"personas"`
}

// PersonaFile is a PersonaStore backed by a single JSON file, written with the same
// temp-fsync-rename dance as the invite store.
//
// The zero value is not usable; call NewPersonaFile.
type PersonaFile struct {
	mu   sync.Mutex
	path string
}

// NewPersonaFile returns a store backed by the file at path. A path that does not
// exist yet reads as an empty store.
func NewPersonaFile(path string) *PersonaFile { return &PersonaFile{path: path} }

// Path reports the file the store is backed by.
func (s *PersonaFile) Path() string { return s.path }

// SetPersona replaces the persona held for a member.
func (s *PersonaFile) SetPersona(ctx context.Context, id domain.MemberID, p Persona) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("enrol: persona for no member")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return err
	}
	if all == nil {
		all = map[domain.MemberID]Persona{}
	}
	all[id] = p
	return writeAtomic(s.path, 0o600, ".personas-*.tmp",
		personaFile{Version: personaFileVersion, Personas: all})
}

// Personas returns every persona held.
func (s *PersonaFile) Personas(ctx context.Context) (map[domain.MemberID]Persona, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// load reads the file. A missing file is an empty store; a malformed one is an
// error, never an empty store, because starting over silently would tell every
// member's agent to forget the name they gave it.
func (s *PersonaFile) load() (map[domain.MemberID]Persona, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("enrol: read %s: %w", s.path, err)
	}
	var f personaFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("enrol: parse %s: %w", s.path, err)
	}
	if f.Version != personaFileVersion {
		return nil, fmt.Errorf("enrol: %s: unsupported format version %d", s.path, f.Version)
	}
	return f.Personas, nil
}
