package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BlueHeisenberg/lore"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Config describes how to reach one lore instance.
//
// One Client speaks to one lore home. LoreHome is per-instance and not a process
// wide setting precisely so that a deployment can run one lore per member pod: the
// home is the only thing that isolates two lore instances on a host.
type Config struct {
	// Command is the lore executable. Nothing on Client needs it: the store is
	// opened in this process and every operation is a library call. It is here
	// for the one thing that is still a subprocess and cannot be anything else —
	// `lore serve`, the sync daemon, which is a long-running process with a
	// lifecycle of its own rather than a call. See sync.go.
	Command string
	// LoreHome is the lore home this client opens. Empty means DefaultLoreHome —
	// $LORE_HOME, then ~/.lore — which is almost never what a deployment wants.
	LoreHome string
	// Env holds extra KEY=VALUE pairs for the sync daemon, appended after the
	// inherited environment and LoreHome, so they win. It has no effect on the
	// store, which is opened in this process.
	Env []string
	// Dir is the sync daemon's working directory.
	Dir string
	// Logger receives lifecycle events. Default: discard.
	Logger *slog.Logger
}

// withDefaults fills in the zero values.
func (c Config) withDefaults() Config {
	if c.LoreHome == "" {
		c.LoreHome = DefaultLoreHome()
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return c
}

// childEnv builds the environment for the sync daemon in sync.go.
func (c Config) childEnv() []string {
	env := os.Environ()
	if c.LoreHome != "" {
		env = append(env, "LORE_HOME="+c.LoreHome)
	}
	return append(env, c.Env...)
}

// Client is a Memory backed by an embedded lore store.
//
// # How it talks to lore
//
// It imports lore and calls it. There is no subprocess, no MCP session and no
// text to parse: lore's Go API returns typed entries and a typed error contract,
// and this file is the translation between that and the vocabulary kenward's
// callers use. Every error it returns is one of this package's sentinels or
// wraps one.
//
// # Operator notes
//
//   - Opening the store does not sync it. Entries written here reach the
//     household's other lore homes only if a `lore serve` is running on this one;
//     writes poke it immediately (lore.Options.NotifyOnWrite) so it does not wait
//     for its next poll. Without a daemon, entries written through this client
//     never leave the machine and entries written elsewhere never arrive. In
//     isolated mode kenward runs one itself — see sync.go.
//   - The home must already be initialised: NewClient on one that is not is
//     ErrStoreUnavailable. In a pod kenward initialises it for itself with
//     lore.Init before this client is built; anywhere else it is `lore init`,
//     run by an operator.
//   - One Client per lore home per process. Two stores on one home contend over
//     one write-ahead log for no gain.
//   - Space display names are neither unique nor stable across lore instances.
//     Everything here is keyed on space ids, which are what kenward configures,
//     and lore compares ids too — so an entry cannot be read or written out of
//     the space the caller named.
type Client struct {
	cfg   Config
	store *lore.Store
}

// NewClient opens the lore home in cfg and returns a Client for it.
//
// Unlike the subprocess client this replaces, it fails here rather than on the
// first call: a home that was never initialised, or one written by a newer lore
// than this build, is ErrStoreUnavailable at construction. That is the better
// place for it — every caller already handles a construction error, and a
// deployment fault should not first present itself as a failed retrieval in the
// middle of somebody's conversation.
func NewClient(cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	if cfg.LoreHome == "" {
		return nil, fmt.Errorf("memory: no lore home: LORE_HOME is unset and this user has no home directory: %w", ErrInvalidArgument)
	}
	st, err := lore.Open(lore.Options{
		Home: cfg.LoreHome,
		// Not optional. kenward writes through this client and `lore serve`
		// carries those writes to the household's other homes; without the poke
		// a write sits here until the daemon's next poll, thirty seconds by
		// default, and nothing anywhere reports that it did.
		NotifyOnWrite: true,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: opening the lore store at %s: %w", cfg.LoreHome, mapErr(err))
	}
	return &Client{cfg: cfg, store: st}, nil
}

// InitHome gives a lore home an account, a device and a personal space, and reports
// whether it made one. A home that already holds a store is left exactly as it was.
//
// It is how kenward needs nothing installed. Every other operation was already a
// library call, but the *first* one was not: a fresh machine has no lore home, lore.Open
// refuses one that was never initialised, and the only documented way to make one was
// `lore init` — an external binary, run by hand, before kenward could do anything at
// all. lore exports Init, so kenward does it for itself, in every path that opens its
// own store: the setup wizard, the dashboard's first-run wizard, and a unit coming up.
//
// # Idempotence is lore's to decide, not this function's
//
// Init refuses a home that already holds an account.json, a device.json or a lore.db
// and returns lore.ErrAlreadyInitialised having written nothing — the rule being "never
// let a new account adopt an existing store", which lore states by naming those three
// files rather than by inferring it from a directory being non-empty. That error is
// this function's created=false, and it is the case that matters most: somebody who
// already runs lore on this machine has kenward join their store, not replace it.
//
// # What it discards, and why
//
// Init returns a recovery code, once, and lore stores it nowhere. It is a KDF factor
// for relay signup and backup, neither of which any kenward deployment configures, and
// the alternative to dropping it is putting a member's account recovery factor into
// `podman logs` — which the operator reads, in the one mode whose whole point is that
// the operator holds nothing of a member's. A member who wants one mints it from inside
// their own pod with `lore recovery new`, which needs no previous code. The account and
// device ids go the same way for the same reason.
//
// # What it does not do
//
// It does not create the spaces kenward.yaml names. Init makes one personal space with
// an id it chooses, and a personal space can never cross accounts; the household's
// shared space and each member's private space are CreateSpace's job, and both wizards
// call it.
func InitHome(ctx context.Context, home, device string) (created bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// The Identity is discarded at the call rather than bound to a variable, so there
	// is nothing here for a later edit to log by reaching for the struct.
	switch _, err := lore.Init(home, device); {
	case err == nil:
		return true, nil
	case errors.Is(err, lore.ErrAlreadyInitialised):
		return false, nil
	default:
		return false, fmt.Errorf("memory: initialising the lore store at %s: %w", home, mapErr(err))
	}
}

var _ Memory = (*Client)(nil)

// Close releases the store. It is idempotent, and every later call returns
// ErrClosed.
func (c *Client) Close() error { return c.store.Close() }

// Search runs one lore search per space and returns the results grouped in the
// order the caller listed the spaces.
//
// Nothing is re-ranked across spaces: lore's relevance ordering holds inside a
// group only. Ranking a private space against a shared one is a policy decision
// that belongs to the assistant.
//
// Every Entry it returns is whole. lore's search result carries the entry's full
// body alongside the snippet that explains the match, so a retrieved entry is the
// entry — not a fragment of it — and it arrives with its origin and its
// timestamps. That was not true of the MCP surface this replaces, which rendered
// the snippet and threw the body away, and the excerpt doctrine that used to live
// here existed only to be honest about it.
//
// Limit applies per space, not to the whole result set, so that a second space
// cannot be crowded out by the first. Zero leaves lore's own default of eight.
func (c *Client) Search(ctx context.Context, q SearchQuery) ([]Entry, error) {
	// The space set is the authorization decision, so it is checked as one: an
	// empty slice and an empty id inside a non-empty slice are the same failure,
	// a call that has not named a space it is entitled to. lore reads an empty
	// space set as "every space this home holds", which is the one thing this
	// client never lets it do.
	if len(q.Spaces) == 0 || slices.Contains(q.Spaces, "") {
		return nil, ErrEmptySpaceSet
	}
	if strings.TrimSpace(q.Text) == "" {
		return nil, fmt.Errorf("memory: search text is empty: %w", ErrInvalidArgument)
	}

	var out []Entry
	for _, sp := range dedupeSpaces(q.Spaces) {
		// lore filters a search by space id in SQL, so a space this home does not
		// hold comes back as no results rather than as an error. That distinction
		// is the whole of `kenward doctor`'s memory check — a display name
		// configured where an id belongs must read as a configuration fault, not
		// as an empty memory — so it is drawn here, before the search.
		if err := c.requireSpace(ctx, sp); err != nil {
			return nil, fmt.Errorf("memory: search space %s: %w", sp, err)
		}
		rs, err := c.store.Search(ctx, q.Text, lore.SearchOpts{
			Spaces: []string{string(sp)},
			Domain: q.Domain,
			Limit:  q.Limit,
		})
		if err != nil {
			return nil, fmt.Errorf("memory: search space %s: %w", sp, mapErr(err))
		}
		// A dropped space would be a silently narrowed answer, so a failure in
		// any one of them fails the search — hence the return above rather than
		// a collected error.
		for _, r := range rs {
			out = append(out, entryOf(r.Entry))
		}
	}
	return out, nil
}

// Get fetches one entry by id from one space.
//
// An entry id must never be taken from member-supplied text: ids originate only
// from a search performed within the current Scope, or from a promotion flow that
// already resolved one.
//
// That rule is the first line of defence and the store is the second. An entry id
// is global to a lore home, so an id is in effect a capability to name an entry in
// any space; lore's scoped read compares the entry's space id to the one asked
// for and refuses a mismatch. An entry that is not in space is ErrNotFound, which
// is also what a deleted one is — a tombstone never comes back from a read.
func (c *Client) Get(ctx context.Context, space domain.SpaceID, id string) (Entry, error) {
	if strings.TrimSpace(id) == "" {
		return Entry{}, fmt.Errorf("memory: entry id is empty: %w", ErrInvalidArgument)
	}
	if err := c.requireSpace(ctx, space); err != nil {
		return Entry{}, err
	}
	e, err := c.store.GetEntryIn(ctx, string(space), id)
	if err != nil {
		return Entry{}, mapErr(err)
	}
	return entryOf(e), nil
}

// Put writes a draft into the given space, always naming the space explicitly so
// that lore's own routing never runs.
//
// lore's defaults apply to anything the Draft leaves empty: confidence
// "provisional" and origin "evidence". Markers are normalised by lore — trimmed,
// upper-cased and bracket-wrapped unless already bracketed — and are passed as a
// slice, so no separator can be embedded in one.
//
// The returned Entry is what lore stored, returned by the write itself. There is
// no read-back and no reconstruction: a commit either happened or returned an
// error, so a write's outcome is never in doubt.
func (c *Client) Put(ctx context.Context, space domain.SpaceID, d Draft) (Entry, error) {
	if strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.Body) == "" || strings.TrimSpace(d.Domain) == "" {
		return Entry{}, fmt.Errorf("memory: title, body and domain are required: %w", ErrInvalidArgument)
	}
	if space == "" {
		return Entry{}, fmt.Errorf("memory: put requires an explicit space: %w", ErrInvalidArgument)
	}
	// The rejected value is not named: a draft is model-generated from a member's
	// conversation, so every one of its fields is content and this error is one a
	// caller may log. The vocabulary it failed against is the diagnostic, and it
	// is constant.
	if d.Confidence != "" && !lore.Confidence(d.Confidence).Valid() {
		return Entry{}, fmt.Errorf("memory: draft confidence is not one of lore's values (experimental, provisional, validated, hardened): %w", ErrInvalidArgument)
	}
	e, err := c.store.PutEntry(ctx, lore.PutParams{
		SpaceID:    string(space),
		Domain:     d.Domain,
		Title:      d.Title,
		Body:       d.Body,
		Markers:    d.Markers,
		Confidence: lore.Confidence(d.Confidence),
	})
	if err != nil {
		return Entry{}, fmt.Errorf("memory: storing an entry in %s: %w", space, mapErr(err))
	}
	return entryOf(e), nil
}

// Share copies an entry from one space to another, preserving lore's provenance.
//
// An entry id must never be taken from member-supplied text: ids originate only
// from a search performed within the current Scope, or from a promotion flow that
// already resolved one.
//
// The source is read scoped to from, so an entry that is not in from is
// ErrNotFound — an id comparison inside the store, not a name check. lore refuses
// to copy entries whose domain begins with profile/ or feedback/ out of the
// personal space, on every path; that comes back as ErrUserModel.
func (c *Client) Share(ctx context.Context, from, to domain.SpaceID, entryID string) (Entry, error) {
	if strings.TrimSpace(entryID) == "" {
		return Entry{}, fmt.Errorf("memory: entry id is empty: %w", ErrInvalidArgument)
	}
	if from == "" || to == "" {
		return Entry{}, fmt.Errorf("memory: share requires both spaces: %w", ErrInvalidArgument)
	}
	if err := c.requireSpace(ctx, from); err != nil {
		return Entry{}, err
	}
	if err := c.requireSpace(ctx, to); err != nil {
		return Entry{}, err
	}
	if _, err := c.store.GetEntryIn(ctx, string(from), entryID); err != nil {
		return Entry{}, mapErr(err)
	}
	e, err := c.store.CopyEntry(ctx, entryID, string(to))
	if err != nil {
		return Entry{}, fmt.Errorf("memory: copying entry %s into %s: %w", entryID, to, mapErr(err))
	}
	return entryOf(e), nil
}

// Delete tombstones one entry in one space.
//
// The space is checked before the delete so that a space this lore home does not
// hold is ErrUnknownSpace rather than a delete attempt, and lore compares space
// ids on the delete itself, so a delete cannot reach out of the space the caller
// named.
//
// Deleting an entry that is already a tombstone returns nil. lore reports it as a
// no-op and it is one; the caller that needs this is undoing a write and wants
// the entry gone, not the honour of being the one who removed it.
func (c *Client) Delete(ctx context.Context, space domain.SpaceID, entryID string) error {
	if strings.TrimSpace(entryID) == "" {
		return fmt.Errorf("memory: entry id is empty: %w", ErrInvalidArgument)
	}
	if space == "" {
		return fmt.Errorf("memory: delete requires an explicit space: %w", ErrInvalidArgument)
	}
	if err := c.requireSpace(ctx, space); err != nil {
		return err
	}
	_, deleted, err := c.store.DeleteEntry(ctx, string(space), entryID)
	if err != nil {
		return fmt.Errorf("memory: deleting entry %s from %s: %w", entryID, space, mapErr(err))
	}
	if !deleted {
		c.cfg.Logger.Info("lore: entry was already deleted", "entry", entryID, "space", string(space))
	}
	return nil
}

// Space is a lore space as lore reports it, for a caller that has to choose one.
//
// It exists because kenward never creates a space in the ordinary course: spaces
// are made out of band, and configuration names them by id. Anything asking an
// operator which space to use must therefore offer what lore already holds rather
// than invent a name — and must be able to tell a personal space from a shared
// one, because lore's personal space never crosses accounts and cannot serve as a
// household's memory.
type Space struct {
	// ID is what kenward is configured with. Names are for humans.
	ID string
	// Name is lore's display name. It is neither unique nor stable.
	Name string
	// Kind is "personal" or "shared", as lore's own enum.
	Kind string
	// Entries is how many live entries lore reports in the space.
	Entries int
}

// Spaces lists the spaces this lore home holds. It is read-only; CreateSpace is
// the only thing here that makes one.
//
// Rows are returned in lore's order, including two spaces sharing one display
// name. lore does not enforce unique names, so the duplicate is the answer — a
// caller showing both with their ids is strictly better than this package
// choosing one of them.
func (c *Client) Spaces(ctx context.Context) ([]Space, error) {
	sps, err := c.store.Spaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory: listing lore spaces: %w", mapErr(err))
	}
	out := make([]Space, 0, len(sps))
	for _, s := range sps {
		// CountEntries counts in SQL; the listing this replaces loaded every body
		// in the home to take a len.
		n, err := c.store.CountEntries(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("memory: counting entries in space %s: %w", s.ID, mapErr(err))
		}
		out = append(out, Space{ID: s.ID, Name: s.Name, Kind: string(s.Kind), Entries: n})
	}
	return out, nil
}

// CreateSpace makes a shared lore space and returns it.
//
// kenward creates a space in one circumstance only — the setup wizard and the
// dashboard's add-a-member flow, where a person has just named a household or a
// member and there is no space for them yet. Everywhere else spaces are
// configured by id and this package only reads them.
//
// A name another space already holds is ErrSpaceExists and nothing is created.
// There is deliberately no get-or-create here: this id becomes a member's private
// space, and quietly handing back an existing space because the names matched is
// how one member's memory becomes another's. The caller asks a person for a
// different name.
func (c *Client) CreateSpace(ctx context.Context, name string) (Space, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return Space{}, fmt.Errorf("memory: a space needs a name: %w", ErrInvalidArgument)
	case strings.ContainsAny(name, "\x00\n\r"):
		// The name comes from a web form, and this is the trust boundary. A
		// newline is not a lore problem — lore would store it — it is a name
		// that cannot be shown on the one line every listing gives it, here and
		// in `lore spaces`.
		return Space{}, fmt.Errorf("memory: a space name cannot contain a newline or a null byte: %w", ErrInvalidArgument)
	case strings.HasPrefix(name, "-"):
		// No longer argv from here — lore is called in-process — but `lore space
		// invite <name>` is the very next command an operator runs against a
		// space kenward made, and lore's flag parser reads a leading dash as a
		// flag.
		return Space{}, fmt.Errorf("memory: a space name cannot start with %q, which lore's own command line would read as a flag: %w", "-", ErrInvalidArgument)
	}
	sp, err := c.store.CreateSpace(ctx, name, lore.Shared)
	if err != nil {
		return Space{}, fmt.Errorf("memory: creating lore space %q: %w", name, mapErr(err))
	}
	return Space{ID: sp.ID, Name: sp.Name, Kind: string(sp.Kind)}, nil
}

// requireSpace fails unless this lore home holds the named space.
//
// It is a fresh lookup every time rather than a cached listing. The listing it
// replaces was cached because each one cost a subprocess round-trip and a parse;
// in-process it is one indexed row read, and not caching it means a space created
// after this client was built is simply found.
func (c *Client) requireSpace(ctx context.Context, space domain.SpaceID) error {
	if space == "" {
		return fmt.Errorf("memory: empty space id: %w", ErrInvalidArgument)
	}
	if _, err := c.store.GetSpace(ctx, string(space)); err != nil {
		// lore's own tools accept either a space id or a display name, so a name
		// reaches here having already worked elsewhere; this is what says which
		// of the two kenward is configured with.
		return fmt.Errorf("memory: lore holds no space %s (spaces are named by id here, not by display name): %w",
			space, mapErr(err))
	}
	return nil
}

// entryOf converts one of lore's entries into kenward's.
//
// Everything kenward models is a field on lore's Entry now, including the origin
// and the timestamps that search used not to report. lore's Version and
// Provenance are dropped rather than carried: nothing in kenward reads them, and
// a field on this struct is a promise to keep filling it.
func entryOf(e lore.Entry) Entry {
	return Entry{
		ID:         e.ID,
		Space:      domain.SpaceID(e.SpaceID),
		Domain:     e.Domain,
		Title:      e.Title,
		Body:       e.Body,
		Confidence: string(e.Confidence),
		Markers:    e.Markers,
		Origin:     string(e.Origin),
		CreatedAt:  loreTime(e.CreatedAt),
		UpdatedAt:  loreTime(e.UpdatedAt),
	}
}

// loreTime parses one of lore's timestamps, tolerating the same-tick padding lore
// appends to keep last-writer-wins monotonic. An unparseable timestamp is the
// zero time: a missing timestamp is a cosmetic loss, and refusing an entry over
// one would be a retrieval failure caused by a clock.
func loreTime(ts lore.Timestamp) time.Time {
	t, err := ts.Time()
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// dedupeSpaces removes repeats while preserving the caller's order.
func dedupeSpaces(in []domain.SpaceID) []domain.SpaceID {
	seen := make(map[domain.SpaceID]bool, len(in))
	out := make([]domain.SpaceID, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
