package remind

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"
)

// storeVersion is written into every file so a future format change can be recognised
// rather than guessed at, exactly as config's state file does it.
const storeVersion = 1

// Defaults applied to a zero-valued Options.
const (
	// DefaultMaxPerDay is how many unprompted messages one unit may send in a day.
	//
	// It is the answer to the question a household actually asks, which is not "how
	// many reminders may I have" but "how often will this thing talk to me without
	// being spoken to". Six is a handful a day: enough for a morning routine and a
	// few one-offs, few enough that a household does not reach for the mute button —
	// and a muted assistant is a dead one.
	DefaultMaxPerDay = 6
	// DefaultMaxStored bounds how many reminders one unit may hold.
	DefaultMaxStored = 20
	// DefaultCatchUp is how late a *repeating* occurrence may be and still be
	// delivered. See Clock.due for why a one-off has no such bound.
	DefaultCatchUp = 6 * time.Hour
)

// Options tune a Store. The zero value is valid and gets the defaults.
type Options struct {
	// Location is the household's timezone. Reminders are stated in wall-clock time
	// and a member means their own clock, not UTC. Nil means time.Local.
	Location *time.Location
	// MaxPerDay caps unprompted messages per day. Zero takes the default; a negative
	// value means no proactive message is ever sent, which is a household's way of
	// turning the whole capability off.
	MaxPerDay int
	// MaxStored caps how many reminders may be held at once.
	MaxStored int
	// CatchUp bounds how late a repeating occurrence may be delivered.
	CatchUp time.Duration
}

func (o Options) normalized() Options {
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.MaxPerDay == 0 {
		o.MaxPerDay = DefaultMaxPerDay
	}
	if o.MaxStored <= 0 {
		o.MaxStored = DefaultMaxStored
	}
	if o.CatchUp <= 0 {
		o.CatchUp = DefaultCatchUp
	}
	return o
}

// contents is the on-disk shape.
type contents struct {
	Version   int        `json:"version"`
	Reminders []Reminder `json:"reminders"`
	// SentDay and SentCount are the day's unprompted-message ledger, in the store's
	// location. They are persisted rather than counted in memory because the cap has
	// to survive a restart: a unit that crash-loops would otherwise reset its own
	// allowance on every boot, which is precisely the run in which a household most
	// needs it not to.
	SentDay   string `json:"sent_day,omitempty"`
	SentCount int    `json:"sent_count,omitempty"`
}

// Store is one unit's durable set of reminders.
//
// It belongs to one unit and is not shared with any other. Two goroutines reach it —
// the unit's turn, when the member sets or cancels one, and the unit's clock, when one
// fires — so it carries a lock; that lock protects one unit's own file and is not
// state keyed by member.
type Store struct {
	mu   sync.Mutex
	path string
	opts Options
	data contents
}

// Open loads a store, creating nothing on disk until something is written.
//
// A missing file is not an error: it is what a unit that has never been asked for a
// reminder looks like. A file that exists but cannot be parsed is an error, because
// starting with an empty set would silently drop every promise the household is owed.
//
// An empty path makes an ephemeral store that keeps its reminders in memory and
// forgets them when the process stops. That is a legitimate thing to ask for and is
// what the assistant's tests use, but it is never what a household wants — a schedule
// that forgets on reboot is not a schedule — so the supervisor builds every production
// store from Config.RemindersPath, which cannot return an empty string.
func Open(path string, opts Options) (*Store, error) {
	s := &Store{path: path, opts: opts.normalized()}
	if path == "" {
		s.data = contents{Version: storeVersion}
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.data = contents{Version: storeVersion}
			return s, nil
		}
		return nil, fmt.Errorf("remind: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return nil, fmt.Errorf("remind: parsing %s: %w", path, err)
	}
	s.data.Version = storeVersion
	return s, nil
}

// Location is the store's timezone.
func (s *Store) Location() *time.Location { return s.opts.Location }

// Add stores a new reminder and returns it with its id filled in.
func (s *Store) Add(r Reminder) (Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Reminders) >= s.opts.MaxStored {
		return Reminder{}, ErrFull
	}
	id, err := newID(func(id string) bool {
		return slices.ContainsFunc(s.data.Reminders, func(x Reminder) bool { return x.ID == id })
	})
	if err != nil {
		return Reminder{}, err
	}
	r.ID = id
	s.data.Reminders = append(s.data.Reminders, r)
	s.sort()
	if err := s.save(); err != nil {
		return Reminder{}, err
	}
	return r, nil
}

// Cancel removes a reminder by id and returns what was removed, so the caller can tell
// the member which one stopped.
func (s *Store) Cancel(id string) (Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Reminders, func(x Reminder) bool { return x.ID == id })
	if i < 0 {
		return Reminder{}, ErrNoSuchReminder
	}
	r := s.data.Reminders[i]
	s.data.Reminders = slices.Delete(s.data.Reminders, i, i+1)
	if err := s.save(); err != nil {
		return Reminder{}, err
	}
	return r, nil
}

// List returns the reminders, soonest first. The slice is a copy.
func (s *Store) List() []Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.data.Reminders)
}

// SentToday reports the day's unprompted-message count against the cap. It is what
// `kenward doctor` prints.
func (s *Store) SentToday(now time.Time) (sent, cap int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.SentDay != s.day(now) {
		return 0, s.opts.MaxPerDay
	}
	return s.data.SentCount, s.opts.MaxPerDay
}

func (s *Store) day(now time.Time) string {
	return now.In(s.opts.Location).Format("2006-01-02")
}

// sort keeps the file and every listing in soonest-first order. Members read the list
// and the order they read it in should be the order things happen.
func (s *Store) sort() {
	slices.SortStableFunc(s.data.Reminders, func(a, b Reminder) int { return a.Next.Compare(b.Next) })
}

// save writes the file atomically: a temporary file in the same directory, flushed,
// then renamed over the target. The pattern is config.State.Save's, for the same
// reason — a truncated file is a household whose reminders have silently vanished, and
// the moment it would happen is a power cut during the one write per change.
func (s *Store) save() error {
	if s.path == "" {
		return nil // ephemeral; see Open
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("remind: creating %s: %w", dir, err)
	}
	s.data.Version = storeVersion
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("remind: encoding: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".reminders-*.json")
	if err != nil {
		return fmt.Errorf("remind: creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := writeAndSync(tmp, data); err != nil {
		tmp.Close()
		return fmt.Errorf("remind: writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("remind: closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("remind: replacing %s: %w", s.path, err)
	}
	syncDir(dir)
	return nil
}

func writeAndSync(f *os.File, data []byte) error {
	// A member's reminders say where they will be and what they are doing. Other
	// users on the machine have no business reading them.
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// syncDir flushes the directory entry so the rename survives a power loss. Best-effort:
// Windows cannot open a directory as a file.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
