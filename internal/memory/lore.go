package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Config describes how to reach one lore instance.
//
// One Client speaks to one lore home. LoreHome is per-instance and not a process
// wide setting precisely so that a deployment can run one lore per member pod:
// LORE_HOME is the only thing that isolates two lore instances on a host.
type Config struct {
	// Command is the lore executable. Required.
	Command string
	// Args are its arguments; kenward passes ["mcp"].
	Args []string
	// LoreHome is exported to the subprocess as LORE_HOME. Empty leaves lore's
	// own default (~/.lore), which is almost never what a deployment wants.
	LoreHome string
	// Env holds extra KEY=VALUE pairs for the subprocess, appended after the
	// inherited environment and LoreHome, so they win.
	Env []string
	// Dir is the subprocess working directory. lore consults its working
	// directory only when a space is not given explicitly, and this client
	// always gives one, so Dir is hygiene rather than routing.
	Dir string
	// CallTimeout bounds a single tool call. Default 15s.
	CallTimeout time.Duration
	// StartTimeout bounds launching the subprocess and completing the MCP
	// handshake. Default 20s.
	StartTimeout time.Duration
	// ShutdownGrace is how long a subprocess is given to exit after its input
	// is closed before it is killed. Default 5s.
	ShutdownGrace time.Duration
	// MaxConcurrent bounds in-flight tool calls. lore serves its SQLite store
	// through a single connection with a five second busy timeout, so a high
	// value buys contention rather than throughput. Default 2.
	MaxConcurrent int
	// BusyRetries is how many times a call is retried after lore reports store
	// contention. Default 3; a negative value disables retrying.
	BusyRetries int
	// Logger receives subprocess lifecycle events. Default: discard.
	Logger *slog.Logger
}

// withDefaults fills in the zero values.
func (c Config) withDefaults() Config {
	if c.CallTimeout <= 0 {
		c.CallTimeout = 15 * time.Second
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 20 * time.Second
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 5 * time.Second
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 2
	}
	if c.BusyRetries < 0 {
		c.BusyRetries = 0
	} else if c.BusyRetries == 0 {
		c.BusyRetries = 3
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return c
}

// childEnv builds the subprocess environment.
func (c Config) childEnv() []string {
	env := os.Environ()
	if c.LoreHome != "" {
		env = append(env, "LORE_HOME="+c.LoreHome)
	}
	return append(env, c.Env...)
}

// Client is a Memory backed by a `lore mcp` subprocess.
//
// # How it talks to lore
//
// lore's MCP server is stdio only and every one of its five tools answers with a
// single block of human-readable text; failures arrive as a successful response
// carrying isError rather than as a protocol error. This client therefore parses
// text (see parse.go) and classifies prose (see errors.go). Output it cannot
// understand becomes a *ParseError naming the expectation that failed — never a
// partly-filled Entry.
//
// # Operator notes
//
//   - `lore mcp` never syncs. It reads and writes the local SQLite store under
//     LORE_HOME and fires one best-effort, one-second poke at a daemon named in
//     LORE_HOME/daemon.json. Without a separate `lore serve` process on the same
//     LORE_HOME, entries written through this client never leave the machine and
//     entries written elsewhere never arrive. Deploy `lore serve` alongside.
//   - `lore mcp` exits before the MCP handshake if LORE_HOME has no account.json
//     and device.json. Run `lore init` first; the subprocess stderr is included
//     in the start-up error to make that diagnosable.
//   - lore has no delete. There is no MCP tool and no CLI command that tombstones
//     an entry, so kenward cannot remove knowledge, only supersede it with a new
//     version. Memory has no Delete method for that reason.
//   - Space display names are not unique and not stable across lore instances.
//     Everything here is keyed on space ids, which are what kenward configures.
//   - The member count printed by lore_spaces is hard-coded to 1 and is neither
//     read nor exposed here.
type Client struct {
	cfg Config
	sem chan struct{}

	mu     sync.Mutex
	cur    *session
	closed bool

	spacesMu   sync.Mutex
	spaceNames map[domain.SpaceID]string
	nameUses   map[string]int
}

// NewClient returns a Client for one lore instance. The subprocess is not started
// until the first call.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("memory: lore command is empty: %w", ErrInvalidArgument)
	}
	cfg = cfg.withDefaults()
	return &Client{cfg: cfg, sem: make(chan struct{}, cfg.MaxConcurrent)}, nil
}

var _ Memory = (*Client)(nil)

// Close terminates the subprocess. It is idempotent, and every later call returns
// ErrClosed.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.cur != nil {
		c.cur.close(c.cfg.ShutdownGrace)
		c.cur = nil
	}
	return nil
}

// Excerpt is one lore search hit. It is deliberately a different type from Entry.
//
// A search result is not a memory: lore_search returns about twelve tokens of the
// body, no origin and no timestamps. An assistant that renders one into a prompt
// as though it were the whole entry is telling the model something false, so the
// two are not interchangeable and converting an Excerpt into an Entry has to be
// written out (see Excerpt.Entry) rather than happening by assignment.
type Excerpt struct {
	// Entry carries what lore_search reports: ID, Space, Domain, Title,
	// Confidence and Markers, with Partial set. Body holds the excerpt with
	// lore's match highlighting removed. Origin, CreatedAt and UpdatedAt are
	// always zero, because lore_search does not report them.
	Entry Entry
	// Snippet is lore's snippet verbatim, with the FTS5 match brackets still in
	// it. It is kept for diagnosing retrieval, not for showing to anyone.
	Snippet string
}

// IsExcerpt reports whether e is a search excerpt rather than a whole entry. It
// is Entry.Partial under a name that states the question being asked.
//
// Prompt rendering, and anything else that presents an entry to the model as a
// complete memory, should check this: an excerpt is about twelve tokens of the
// body, and presenting it as the whole thing tells the model something false.
func IsExcerpt(e Entry) bool { return e.Partial }

// Search implements Memory. It is a thin wrapper over SearchExcerpts, kept
// because the interface is expressed in entries; prefer SearchExcerpts, whose
// type says what these values actually are.
//
// Every Entry it returns has Partial set: Body is an excerpt of about twelve
// tokens, and Origin, CreatedAt and UpdatedAt are zero. The flag is what survives
// into a []Entry, where the Excerpt type no longer can. Call Get with the id for
// the real entry before presenting one as a memory.
func (c *Client) Search(ctx context.Context, q SearchQuery) ([]Entry, error) {
	xs, err := c.SearchExcerpts(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.Entry)
	}
	return out, nil
}

// SearchExcerpts runs one lore_search per space, concurrently, and returns the
// results grouped in the order the caller listed the spaces.
//
// Nothing is re-ranked across spaces: lore's relevance ordering holds inside a
// group only. Ranking a private space against a shared one is a policy decision
// that belongs to the assistant.
//
// Excerpt.Entry.Body is lore's FTS5 snippet with the match brackets stripped —
// roughly twelve tokens of the body, with an ellipsis where text was elided. The
// stripping cannot distinguish a highlight bracket from a bracket that was in the
// body, so the untouched snippet is kept on Excerpt.Snippet.
//
// Limit applies per space, not to the whole result set, so that a second space
// cannot be crowded out by the first. Zero leaves lore's own default of eight.
func (c *Client) SearchExcerpts(ctx context.Context, q SearchQuery) ([]Excerpt, error) {
	if len(q.Spaces) == 0 {
		return nil, ErrEmptySpaceSet
	}
	if strings.TrimSpace(q.Text) == "" {
		return nil, fmt.Errorf("memory: search text is empty: %w", ErrInvalidArgument)
	}
	spaces := dedupeSpaces(q.Spaces)

	groups := make([][]Excerpt, len(spaces))
	errs := make([]error, len(spaces))
	var wg sync.WaitGroup
	for i, sp := range spaces {
		wg.Add(1)
		go func() {
			defer wg.Done()
			groups[i], errs[i] = c.searchSpace(ctx, sp, q)
		}()
	}
	wg.Wait()
	// A dropped space would be a silently narrowed answer, so a failure in any
	// one of them fails the search.
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	var out []Excerpt
	for _, g := range groups {
		out = append(out, g...)
	}
	return out, nil
}

// searchSpace runs lore_search against exactly one space.
func (c *Client) searchSpace(ctx context.Context, space domain.SpaceID, q SearchQuery) ([]Excerpt, error) {
	args := map[string]any{
		"query": q.Text,
		"space": string(space),
	}
	if q.Domain != "" {
		args["domain"] = q.Domain
	}
	if q.Limit > 0 {
		args["limit"] = q.Limit
	}
	text, err := c.callTool(ctx, toolSearch, args, true)
	if err != nil {
		return nil, fmt.Errorf("memory: search space %s: %w", space, err)
	}
	xs, err := parseSearch(text)
	if err != nil {
		return nil, err
	}
	for i := range xs {
		xs[i].Entry.Space = space
	}
	return xs, nil
}

// Get fetches one entry by id and confirms it lives in the given space.
//
// An entry id must never be taken from member-supplied text: ids originate only
// from a search performed within the current Scope, or from a promotion flow that
// already resolved one.
//
// That rule is the first line of defence, and this is the second. lore_get is not
// space-scoped — an entry id is globally unique in a lore store and lore will
// happily return an entry from any space, so an id is in effect a capability to
// read any space — so this client resolves the space id to its display name
// through lore_spaces and rejects a mismatch as ErrNotFound. The check is only as
// strong as lore's naming: if two spaces in the same store carry the same display
// name it cannot tell them apart, which is why it is not the primary defence.
//
// Entry.CreatedAt is always zero; lore's MCP surface never reports created_at.
// lore also returns tombstoned entries by id and does not mark them, so a deleted
// entry is indistinguishable from a live one here.
func (c *Client) Get(ctx context.Context, space domain.SpaceID, id string) (Entry, error) {
	if strings.TrimSpace(id) == "" {
		return Entry{}, fmt.Errorf("memory: entry id is empty: %w", ErrInvalidArgument)
	}
	want, err := c.spaceNameFor(ctx, space)
	if err != nil {
		return Entry{}, err
	}
	r, err := c.getRendered(ctx, id)
	if err != nil {
		return Entry{}, err
	}
	if r.SpaceName != want {
		return Entry{}, fmt.Errorf("memory: entry %s is in lore space %q, not %q: %w",
			id, r.SpaceName, want, ErrNotFound)
	}
	r.Entry.Space = space
	return r.Entry, nil
}

// getRendered fetches an entry by id without checking which space it is in.
func (c *Client) getRendered(ctx context.Context, id string) (rendered, error) {
	text, err := c.callTool(ctx, toolGet, map[string]any{"id": id}, true)
	if err != nil {
		return rendered{}, err
	}
	return parseEntry(text)
}

// Put writes a draft into the given space, always naming the space explicitly so
// that lore's subject routing and working-directory lookup never run.
//
// lore's own defaults apply to anything the Draft leaves empty: confidence
// "provisional" and origin "evidence". Markers are normalised by lore — trimmed,
// upper-cased and bracket-wrapped unless already bracketed — and are passed as a
// comma-separated list, so a marker containing a comma is rejected here rather
// than silently split.
//
// The returned Entry is read back from lore so that it carries the stored form
// rather than the draft. If the read-back fails the write still succeeded, and
// the Entry is reconstructed from the draft and the write receipt, with
// UpdatedAt left zero.
//
// On failure the caller must distinguish two cases before saying anything to a
// member. An error matching ErrWriteUncertain means the request reached lore and
// the answer did not come back: the entry may exist. Anything else means nothing
// was written. lore has no delete, so a retry after an uncertain write can leave
// a permanent duplicate.
func (c *Client) Put(ctx context.Context, space domain.SpaceID, d Draft) (Entry, error) {
	if strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.Body) == "" || strings.TrimSpace(d.Domain) == "" {
		return Entry{}, fmt.Errorf("memory: title, body and domain are required: %w", ErrInvalidArgument)
	}
	if space == "" {
		return Entry{}, fmt.Errorf("memory: put requires an explicit space: %w", ErrInvalidArgument)
	}
	args := map[string]any{
		"title":  d.Title,
		"body":   d.Body,
		"domain": d.Domain,
		"space":  string(space),
	}
	if d.Confidence != "" {
		if !confidences[d.Confidence] {
			return Entry{}, fmt.Errorf("memory: %q is not a lore confidence value: %w", d.Confidence, ErrInvalidArgument)
		}
		args["confidence"] = d.Confidence
	}
	if len(d.Markers) > 0 {
		for _, m := range d.Markers {
			if strings.Contains(m, ",") {
				return Entry{}, fmt.Errorf("memory: marker %q contains a comma, which lore uses as its marker separator: %w", m, ErrInvalidArgument)
			}
		}
		args["markers"] = strings.Join(d.Markers, ",")
	}

	// Not retried on a lost subprocess: a write that may have landed must not be
	// replayed. Store contention is still retried, because lore reports it
	// before anything is committed.
	text, err := c.callTool(ctx, toolPut, args, false)
	if err != nil {
		return Entry{}, err
	}
	st, err := parseStored(text)
	if err != nil {
		// lore answered, so the entry is stored; this client just cannot read
		// the receipt well enough to name it. From the member's side that is
		// the same situation as a lost answer.
		return Entry{}, fmt.Errorf("%w: %w", ErrWriteUncertain, err)
	}

	fallback := Entry{
		ID:         st.ID,
		Space:      space,
		Domain:     st.Domain,
		Title:      d.Title,
		Body:       d.Body,
		Confidence: st.Confidence,
		Origin:     st.Origin,
		Markers:    normalizeMarkers(d.Markers),
		// Whole, not partial: Body is the draft's own text. Only UpdatedAt is
		// missing, and that is absence of a timestamp, not elision of content.
		Partial: false,
	}
	r, err := c.getRendered(ctx, st.ID)
	if err != nil {
		c.cfg.Logger.Warn("lore: stored entry could not be read back", "entry", st.ID, "err", err)
		return fallback, nil
	}
	r.Entry.Space = space
	return r.Entry, nil
}

// Share copies an entry from one space to another, preserving lore's provenance.
//
// An entry id must never be taken from member-supplied text: ids originate only
// from a search performed within the current Scope, or from a promotion flow that
// already resolved one.
//
// lore_share takes no source space, so the source is fetched first and its space
// checked against from; an entry that is not in from is reported as ErrNotFound,
// for the same reason Get does it, and with the same caveat — it is the second
// line of defence, not the first. The copy is executed directly with
// confirm:true — lore's two-phase preview is an affordance for an agent deciding
// whether to share, and kenward has already taken that decision through its own
// confirmation before calling here.
//
// lore refuses to copy entries whose domain begins with profile/ or feedback/ out
// of the personal space, on every path; that comes back as ErrUserModel.
//
// As with Put, a failure matching ErrWriteUncertain means the copy may exist and
// must not be retried blindly; anything else means nothing was copied.
func (c *Client) Share(ctx context.Context, from, to domain.SpaceID, entryID string) (Entry, error) {
	if strings.TrimSpace(entryID) == "" {
		return Entry{}, fmt.Errorf("memory: entry id is empty: %w", ErrInvalidArgument)
	}
	if from == "" || to == "" {
		return Entry{}, fmt.Errorf("memory: share requires both spaces: %w", ErrInvalidArgument)
	}
	fromName, err := c.spaceNameFor(ctx, from)
	if err != nil {
		return Entry{}, err
	}
	if _, err := c.spaceNameFor(ctx, to); err != nil {
		return Entry{}, err
	}
	src, err := c.getRendered(ctx, entryID)
	if err != nil {
		return Entry{}, err
	}
	if src.SpaceName != fromName {
		return Entry{}, fmt.Errorf("memory: entry %s is in lore space %q, not %q: %w",
			entryID, src.SpaceName, fromName, ErrNotFound)
	}

	text, err := c.callTool(ctx, toolShare, map[string]any{
		"entry_id": entryID,
		"to_space": string(to),
		"confirm":  true,
	}, false)
	if err != nil {
		return Entry{}, err
	}
	cp, err := parseCopied(text)
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %w", ErrWriteUncertain, err)
	}

	r, err := c.getRendered(ctx, cp.NewID)
	if err != nil {
		// The copy exists; only its stored timestamps are unknown.
		c.cfg.Logger.Warn("lore: shared entry could not be read back", "entry", cp.NewID, "err", err)
		e := src.Entry
		e.ID = cp.NewID
		e.Space = to
		e.UpdatedAt = time.Time{}
		return e, nil
	}
	r.Entry.Space = to
	return r.Entry, nil
}

// spaceNameFor resolves a space id to lore's display name for it, refreshing the
// cached listing once on a miss so that a space created after this client started
// is still found.
func (c *Client) spaceNameFor(ctx context.Context, space domain.SpaceID) (string, error) {
	if space == "" {
		return "", fmt.Errorf("memory: empty space id: %w", ErrInvalidArgument)
	}
	c.spacesMu.Lock()
	name, ok := c.spaceNames[space]
	c.spacesMu.Unlock()
	if ok {
		return name, nil
	}
	if err := c.refreshSpaces(ctx); err != nil {
		return "", err
	}
	c.spacesMu.Lock()
	defer c.spacesMu.Unlock()
	name, ok = c.spaceNames[space]
	if !ok {
		return "", fmt.Errorf("memory: lore holds no space %s: %w", space, ErrUnknownSpace)
	}
	if c.nameUses[name] > 1 {
		c.cfg.Logger.Warn("lore: space display name is ambiguous, entry space checks are weakened",
			"name", name, "spaces", c.nameUses[name])
	}
	return name, nil
}

// refreshSpaces reloads the space id to display name mapping from lore_spaces.
func (c *Client) refreshSpaces(ctx context.Context) error {
	text, err := c.callTool(ctx, toolSpaces, map[string]any{}, true)
	if err != nil {
		return err
	}
	rows, err := parseSpaces(text)
	if err != nil {
		return err
	}
	names := make(map[domain.SpaceID]string, len(rows))
	uses := make(map[string]int, len(rows))
	for _, r := range rows {
		names[domain.SpaceID(r.ID)] = r.Name
		uses[r.Name]++
	}
	c.spacesMu.Lock()
	c.spaceNames, c.nameUses = names, uses
	c.spacesMu.Unlock()
	return nil
}

// deadGrace is how long a failed call waits for the session to confirm it died,
// so that a subprocess that exited mid-call is recognised as such rather than
// reported as an opaque transport error.
const deadGrace = 200 * time.Millisecond

// callTool makes one lore tool call, restarting the subprocess when it has died
// and retrying bounded, safe failures.
//
// idempotent marks a call that may be replayed on a fresh subprocess. Writes are
// not idempotent: a lore_put whose response was lost may already have landed, so
// it is reported rather than repeated, and the next call restarts the subprocess.
// A non-idempotent call that fails after the request went out is wrapped in
// ErrWriteUncertain; a rejection lore itself sent back is not, because lore
// rejecting a call means it did not apply it.
func (c *Client) callTool(ctx context.Context, tool string, args map[string]any, idempotent bool) (string, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-c.sem }()

	attempts := c.cfg.BusyRetries + 1
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return "", err
			}
		}
		s, err := c.acquire(ctx)
		if err != nil {
			return "", err
		}
		callCtx, cancel := context.WithTimeout(ctx, c.cfg.CallTimeout)
		res, err := s.mcp.CallTool(callCtx, &mcp.CallToolParams{Name: tool, Arguments: args})
		timedOut := callCtx.Err() != nil && ctx.Err() == nil
		cancel()

		// Once the request is on the wire, an error that is not lore's own
		// answer leaves a write's outcome unknown: lore may have committed it
		// and lost the reply. Reads have no such problem.
		uncertain := func(err error) error {
			if idempotent {
				return err
			}
			return fmt.Errorf("%w: %w", ErrWriteUncertain, err)
		}

		switch {
		case err != nil && ctx.Err() != nil:
			return "", uncertain(fmt.Errorf("memory: %s: %w", tool, ctx.Err()))
		case err != nil && timedOut:
			return "", uncertain(fmt.Errorf("memory: %s: no answer within %s: %w", tool, c.cfg.CallTimeout, err))
		case err != nil:
			if !s.waitDead(deadGrace) {
				return "", uncertain(fmt.Errorf("memory: %s: %w", tool, err))
			}
			c.discard(s)
			c.cfg.Logger.Warn("lore: subprocess ended", "tool", tool, "err", err)
			ended := fmt.Errorf("memory: %s: lore subprocess ended: %w%s", tool, err, s.proc.stderrSuffix())
			if !idempotent {
				return "", uncertain(ended)
			}
			lastErr = ended
			continue
		}

		if res.IsError {
			terr := toolError(tool, resultText(res))
			if errors.Is(terr, ErrBusy) {
				lastErr = terr
				continue
			}
			return "", terr
		}
		return resultText(res), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("memory: %s: exhausted retries", tool)
	}
	return "", lastErr
}

// acquire returns the live session, starting or restarting the subprocess as
// needed. A subprocess that has died is replaced here rather than remembered as a
// permanent failure.
func (c *Client) acquire(ctx context.Context) (*session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.cur != nil {
		if c.cur.alive() {
			return c.cur, nil
		}
		c.cfg.Logger.Info("lore: restarting subprocess", "command", c.cfg.Command)
		c.cur.close(c.cfg.ShutdownGrace)
		c.cur = nil
	}
	s, err := dial(ctx, c.cfg)
	if err != nil {
		return nil, err
	}
	c.cur = s
	return s, nil
}

// discard retires a session if it is still the current one.
func (c *Client) discard(s *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == s {
		c.cur = nil
	}
	s.close(c.cfg.ShutdownGrace)
}

// resultText joins the text blocks of a tool result. lore always returns exactly
// one; joining rather than requiring one keeps a future second block from being
// dropped silently.
func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeMarkers mirrors lore's marker normalisation so that a reconstructed
// Entry matches what lore stored.
func normalizeMarkers(in []string) []string {
	var out []string
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if !strings.HasPrefix(m, "[") {
			m = "[" + strings.ToUpper(m) + "]"
		}
		out = append(out, m)
	}
	return out
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

// backoff is the delay before retry number n, starting at n = 1.
func backoff(n int) time.Duration {
	d := 50 * time.Millisecond << (n - 1)
	if d > time.Second {
		d = time.Second
	}
	return d
}

// sleepCtx sleeps unless the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
