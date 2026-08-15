# kenward — implementation contract

This document is the build contract. Every package below is implemented against the
interfaces and semantics defined here; where an implementation disagrees with this
document, the document is wrong and gets fixed deliberately rather than drifted from.

Companion documents: `ARCHITECTURE.md` (why), this file (what and how).

---

## 0. Ground rules

- **Go 1.25.** No cgo. Cross-platform (Windows / macOS / Linux) everywhere except
  `supervisor/isolated`, which is Linux-only and must degrade with a clear error, never
  a panic, elsewhere.
- **The per-member assistant is an isolated unit.** No shared mutable state keyed by
  member id, anywhere, ever. This is what allows the same code to run as N goroutines
  in Simple mode and N processes in Isolated mode. It cannot be retrofitted.
- **`context.Context` is the first argument** of anything that can block.
- **No global state**, no package-level singletons, no `init()` side effects.
- **Typed sentinel errors** for anything the caller must distinguish
  (`ErrNoBackend`, `ErrNotEnrolled`, `ErrLocked`, …). Wrap with `%w`.
- **Every package has table-driven tests.** Unit tests make no network calls and touch
  no real Telegram, provider or lore instance — all three are interfaces with fakes.
- **Nothing writes to memory without an explicit user confirmation.** No exceptions, no
  config flag to disable the confirmation.

---

## 1. Package tree

```
cmd/kenward/            entrypoint: run, setup, version, doctor
internal/domain/        core types. Depends on nothing.
internal/config/        YAML load, defaults, validation
internal/scope/         message -> Scope resolution. THE authorization boundary.
internal/memory/        Memory interface + lore MCP client
internal/transport/     Transport interface + Telegram implementation + Mux
internal/routing/       tier chain, endpoint pool, probe, cooldown, failover
internal/session/       key unwrap, idle expiry
internal/capture/       capture proposal + confirmation state machine
internal/enrol/         claim codes, onboarding state machine
internal/assistant/     the per-member Unit: one turn, end to end
internal/supervisor/    simple (goroutines) | isolated (pods)
internal/setup/         first-run wizard
internal/version/       build metadata
```

Dependency direction is strictly downward: `domain` depends on nothing; `assistant`
depends on the interfaces, never on their implementations. `supervisor` wires
concrete implementations together and is the only package that may import all of them.

Third-party dependencies, fixed:

| Module | Version | Used by |
| --- | --- | --- |
| `github.com/go-telegram/bot` | v1.23.0 | `transport` |
| `github.com/modelcontextprotocol/go-sdk` | v1.7.0 | `memory` |
| `gopkg.in/yaml.v3` | v3.0.1 | `config` |
| `github.com/BlueHeisenberg/keel` | — | `routing` (llm), `session` (vault), `supervisor` (sandbox), update |

Note: the MCP SDK requires Go 1.25, which is why the module targets it.

Adding a dependency outside this table requires a decision recorded in
`ARCHITECTURE.md`, not a `go get`.

---

## 2. Domain types — `internal/domain`

```go
type MemberID string   // stable internal id; NOT the Telegram id
type SpaceID string    // lore space identifier, always passed explicitly

type Member struct {
    ID         MemberID
    Name       string
    TelegramID int64     // 0 until claimed
    Private    SpaceID
    Tiers      []string  // ordered tier chain for this member's private conversations
    EnrolledAt time.Time
}

type Household struct {
    Name        string
    Shared      SpaceID
    GroupChatID int64
    Tiers       []string // ordered tier chain for the group conversation
}

type ScopeKind int
const (
    ScopeDirect ScopeKind = iota + 1
    ScopeGroup
)

// Scope is the resolved answer to "who is this, and what may this conversation touch".
// Producing a Scope IS the authorization decision. Everything downstream obeys it and
// re-derives nothing.
type Scope struct {
    Kind      ScopeKind
    Member    *Member    // nil iff Kind == ScopeGroup
    Write     SpaceID    // where captures land
    Read      []SpaceID  // ordered: primary first
    Tiers     []string   // ordered tier chain
    ChatID    int64
}
```

**The invariants, which are tested directly and must never be weakened:**

| Kind | `Read` | `Write` | May offer "personal" capture |
| --- | --- | --- | --- |
| `ScopeDirect` | `[member.Private, household.Shared]` | `member.Private` | yes |
| `ScopeGroup` | `[household.Shared]` | `household.Shared` | **no** |

A group `Scope` must never contain any member's private `SpaceID` in `Read` or `Write`.
`scope` package has a test asserting exactly this over generated configurations.

---

## 3. The seams

### `internal/memory`

```go
type Entry struct {
    ID         string
    Space      SpaceID
    Domain     string
    Title      string
    Body       string
    Confidence string    // lore's vocabulary; validated against config.AllowedConfidence
    Markers    []string
    Origin     string
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

type Draft struct {
    Domain     string
    Title      string
    Body       string
    Confidence string
    Markers    []string
}

type SearchQuery struct {
    Text   string
    Spaces []SpaceID   // REQUIRED and explicit. An empty slice is an error, never "all".
    Domain string
    Limit  int
}

type Memory interface {
    Search(ctx context.Context, q SearchQuery) ([]Entry, error)
    Get(ctx context.Context, space SpaceID, id string) (Entry, error)
    Put(ctx context.Context, space SpaceID, d Draft) (Entry, error)
    Share(ctx context.Context, from, to SpaceID, entryID string) (Entry, error)
    Close() error
}

var ErrEmptySpaceSet = errors.New("memory: search requires an explicit space set")
```

`SearchQuery.Spaces` being required is the mechanical guarantee that no code path can
accidentally search everything. The lore client returns `ErrEmptySpaceSet` rather than
defaulting.

Results from a multi-space search are returned **grouped in the order of
`q.Spaces`**, not globally re-ranked — ranking across spaces is a policy decision that
belongs to the assistant, not the client.

### `internal/transport`

```go
type Inbound struct {
    ChatID    int64
    UserID    int64
    Text      string
    MessageID int
    IsGroup   bool
    At        time.Time
}

type Outbound struct {
    ChatID  int64
    Text    string
    ReplyTo int   // 0 for none
}

type Choice struct {
    ID    string   // stable, machine-readable
    Label string   // shown on the button
}

type Question struct {
    ChatID        int64
    Text          string
    Choices       []Choice
    AllowedUserID int64          // ONLY this user's taps are accepted
    Timeout       time.Duration  // on expiry: treated as declined
}

type Answer struct {
    ChoiceID string
    UserID   int64
    TimedOut bool
}

type Transport interface {
    Updates(ctx context.Context) (<-chan Inbound, error)
    Send(ctx context.Context, o Outbound) error
    Ask(ctx context.Context, q Question) (Answer, error)
    Close() error
}
```

`AllowedUserID` is load-bearing: in a group chat, any member can see and tap an inline
keyboard. A tap from anyone else is ignored — not answered, not acknowledged. Without
this, another member could route someone's capture.

`Mux` fans a single bot's `Updates` channel out to per-member scoped `Transport` views
by `(UserID, ChatID)`. Simple mode uses it; Isolated mode does not (each pod owns its
own bot).

### `internal/routing`

```go
type Endpoint struct {
    Name      string
    BaseURL   string
    Model     string
    APIKeyEnv string    // env var name; never the key itself
    Tags      []string  // tier names
    Timeout   time.Duration
}

type Request struct {
    Messages    []llm.Message
    MaxTokens   int
    Temperature float64
}

type Router interface {
    Complete(ctx context.Context, chain []string, req Request) (llm.Completion, error)
}

// ErrNoBackend carries what was tried, so the refusal can be specific.
type NoBackendError struct{ Chain []string; Tried []string }
func (e *NoBackendError) Error() string
```

**Semantics, in order:**

1. For each tier name in `chain`, in order:
   1. Candidates = endpoints tagged with that tier, minus endpoints in cooldown.
   2. Each candidate is **connect-probed**: TCP dial to host:port, 2s timeout, result
      cached for 10s per host:port. A machine that is powered off is skipped here
      rather than hanging on the OS connect timeout.
   3. Surviving candidates are tried in least-recently-used order.
   4. First endpoint to produce a response wins.
2. If a tier yields nothing, fall through to the next tier in `chain`.
3. If every tier is exhausted, return `*NoBackendError`. **Never** silently widen the
   chain — the chain is the privacy policy.

**Failover happens before the first token only.** Once a response has begun, an error
is returned to the caller as a partial failure; there is no retry, because retrying
produces spliced or duplicated output.

**Cooldown:** an endpoint that errors is cooled for 30s, doubling per consecutive
failure to a 5m ceiling, reset on success.

### `internal/session`

```go
type Sessions interface {
    Unlock(ctx context.Context, id MemberID, passphrase string) error
    Key(id MemberID) ([]byte, bool)
    Touch(id MemberID)
    Lock(id MemberID)
    LockAll()
}
```

Keys are unwrapped into memory only, never written to disk, and zeroed on `Lock`. Idle
expiry defaults to 30 minutes, configurable. `LockAll` runs on shutdown signals.

Mode difference, stated honestly in the docs and in `kenward doctor` output:

- **Simple:** one operator-held node passphrase wraps every member key. The operator
  can unlock anyone. This is the mode's known limitation, not a bug.
- **Isolated:** each member's key is wrapped by their own passphrase and lives only in
  their pod.

---

## 4. Configuration

`kenward.yaml`, validated on load; unknown fields are an error, not a warning.

```yaml
mode: simple                  # simple | isolated

household:
  name: "Home"
  shared_space: household
  group_chat_id: -1001234567890
  tiers: [local, local-slow, cloud]

telegram:
  # simple mode: one token for everything
  bot_token_env: KENWARD_BOT_TOKEN
  # isolated mode: per-member tokens, see members[].bot_token_env

members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: david-private
    tiers: [local]            # local-only: refuses rather than reaching for cloud
    bot_token_env: KENWARD_BOT_TOKEN_DAVID   # isolated mode only

endpoints:
  - name: monster
    base_url: http://monster.tail:8000/v1
    model: qwen3.6-27b-awq
    tags: [local]
    timeout: 120s
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-sonnet-5
    api_key_env: OPENROUTER_API_KEY
    tags: [cloud]

memory:
  lore_command: ["lore", "mcp"]
  search_limit: 8

session:
  idle_timeout: 30m

capture:
  max_proposals_per_turn: 1

update:
  channel: stable             # stable | edge | off
  check_interval: 6h
```

Validation rules that are errors, not warnings:

- every tier named in `household.tiers` or `members[].tiers` must be a tag on at least
  one endpoint
- `private_space` values must be unique across members and must not equal
  `household.shared_space`
- `telegram_id` values must be unique
- in `isolated` mode every member must have a distinct `bot_token_env`
- referenced env vars must be set at startup, or `kenward` exits with a list of what is
  missing — never starts half-configured

---

## 5. The turn

`internal/assistant.Unit.Handle(ctx, in Inbound) error`:

1. **Resolve scope.** `scope.Resolve(cfg, in)`. Unknown sender or unmapped chat →
   return `ErrNotEnrolled`; the caller drops it silently, sending nothing. Never reply
   "you are not authorised" — that confirms the bot exists to a stranger.
2. **Ensure session.** If locked → prompt for unlock and stop.
3. **Retrieve.** One `memory.Search` per space in `scope.Read`, concurrently, results
   kept grouped in scope order. Budget: `memory.search_limit` per space.
4. **Assemble.** System prompt + retrieved entries (rendered with their markers and
   confidence) + the last N turns from the unit-local history ring.
5. **Route.** `router.Complete(ctx, scope.Tiers, req)`. A `*NoBackendError` becomes an
   explicit refusal naming the tiers tried — never a silent fallback.
6. **Reply.**
7. **Capture.** If the model proposed a memory write, run the capture state machine.
8. **Record** the turn in the unit-local history ring.

History is unit-local, in memory, bounded (default 20 turns), and is **not** written to
lore. lore holds distilled knowledge, not transcripts.

---

## 6. Capture

Model proposals arrive as a structured tool call: `{title, body, domain, confidence,
markers, target: personal|shared|unsure}`.

| Situation | Behaviour |
| --- | --- |
| Direct chat, target `unsure` | Ask: `[Personal] [Household] [Don't save]` |
| Direct chat, target known | Ask to confirm: `[Save to X] [Don't save]` |
| Group chat, any target | Ask: `[Household] [Don't save]` — **"Personal" is never offered** |
| Promotion of an existing private entry to shared | Separate flow: show the full text that will be published, then `[Publish to household] [Cancel]` |

Rules:

- At most `capture.max_proposals_per_turn` (default 1) proposals per turn.
- A proposal whose title matches one declined in the last 10 turns is suppressed.
- Timeout on the question is treated as **declined**, never as accepted.
- The answer is only accepted from `AllowedUserID`.
- Promotion uses `memory.Share`, never a read-then-put, so lore's own provenance is
  preserved.

---

## 7. Enrolment

Telegram bot usernames are publicly discoverable and anyone may `/start`. Therefore:

1. Operator runs `kenward invite --name "David"` → prints a single-use claim code with
   an expiry (default 24h). Codes are stored hashed.
2. A stranger messaging the bot gets **no reply at all** until a valid code is
   presented. Not an error, not a prompt — silence.
3. On a valid code: bind `telegram_id` → member, create/attach the private space, mark
   the code consumed, and run the short onboarding explaining the two memories and how
   capture works.
4. Codes are single-use, expiring, rate-limited (5 attempts per chat per hour) and
   compared in constant time.

Removal is `kenward revoke <member>`; it unbinds the Telegram id and reports that the
space key must be rotated in lore.

---

## 8. Modes and the supervisor

```go
type Supervisor interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Health(ctx context.Context) ([]UnitHealth, error)
}
```

- **`simple`**: one process. One `Transport`, a `Mux` fanning updates to one `Unit` per
  member plus one group `Unit`. Everything in one address space — the mode's stated
  limitation.
- **`isolated`**: one pod per member plus one for the group, via `keel/sandbox`. Each
  pod holds its own bot token, its own lore instance and its own key. Linux only.
  Restarts are per-pod, so one crashing member does not take the household down.

The `Unit` implementation is identical in both. If a change to `Unit` needs to know
which mode it is in, that change is wrong.

---

## 9. Update

`keel/update`, driven by config. Requirements are absolute:

- signature verified against a public key compiled into the binary; unsigned or
  mismatched artifacts are refused, not warned about
- download → verify → swap → restart → health check within 30s → keep, else
  **automatic rollback** to the retained previous version
- health = process up, lore MCP responds, Telegram authorises. **Endpoint reachability
  is deliberately not part of health** — machines being asleep is normal and would
  cause endless rollbacks
- never restart mid-conversation: drain until no unit has an active turn
- patch/minor auto-apply; **major asks first, over Telegram**
- a release may never silently change routing policy or tier configuration; if defaults
  move, consent is required
- `channel: off` is fully supported and the product works forever without updating
- Isolated mode updates pods **one member at a time**

---

## 10. Refusals

Refusal text is a product surface. It must say what happened and why, and never imply a
capability that does not exist:

> No machine in your allowed tiers (`local`) is reachable right now — I tried `monster`
> and `5090`. Your private space is set to local-only, so I won't send this to a cloud
> provider. Wake one of them and ask again.

Refusals are golden-tested. Changing one is a deliberate edit to a test fixture.

---

## 11. Testing

- `scope` has an exhaustive table test: `(config, inbound) -> Scope | rejection`. This
  is the security test of the whole product; it must cover unknown users, unknown
  chats, a member messaging from a different chat, and a group id that collides with a
  member's private space.
- `routing` is tested against a fake endpoint set: cooldown behaviour, probe caching,
  tier fallthrough, and the assertion that an exhausted chain **never** reaches an
  endpoint outside it.
- `capture` is tested as a state machine including timeout-as-decline and the
  taps-from-the-wrong-user case.
- `memory` is tested against a scripted fake MCP server, not a live lore.
- Refusal strings are golden files.

Integration tests requiring real Podman, a real lore or a real provider are tagged
`//go:build integration` and excluded from the default `go test ./...`.

---

## 12. What lore actually does

Established by reading lore's source, not by assumption. These facts constrain the
design and several of them contradict what the architecture originally supposed.

- **`lore mcp` is stdio only**, exposes five tools (`lore_search`, `lore_get`,
  `lore_put`, `lore_spaces`, `lore_share`), and returns **unstructured text**, not JSON.
  Failures arrive as `isError: true` rather than as protocol errors. The client is
  therefore a parser, and is tested against a golden corpus so that a format change
  fails loudly in one place.
- **There is no Go client package.** Everything in lore is under `internal/`, so kenward
  speaks MCP over stdio.
- **Private memory must be a `shared`-kind space with two members.** lore's `personal`
  space never crosses accounts, so a node could not read it. This is what the
  architecture already specified, now confirmed as the only workable option rather than
  a preference.
- **Instances are isolated by `LORE_HOME`, not by machine.** Several lore daemons can
  run on one host, each holding a subset of spaces, and converge on a shared space.
  This is what makes one lore per member pod viable in isolated mode.
- **Sync is last-writer-wins per entry**, compared on `(updated_at, author_account)`.
  It is not a CRDT: the losing version is discarded silently, with no conflict record.
  A machine with a fast clock wins every conflict. Household clocks should be synced,
  and nothing in kenward may assume a write it made is still there.
- **`lore mcp` alone never syncs.** Syncing requires a separate `lore serve`. Any
  deployment running more than one lore instance must run both.
- **Invites are not exposed over MCP.** Enrolment drives the lore CLI, which has
  non-interactive flags but emits no JSON.
- **There is no delete.** Anything kenward stores is permanent from lore's side.
- `confidence` ∈ {experimental, provisional, validated, hardened} and `origin` ∈
  {evidence, directive, convention, constraint}, both enforced by lore. **Markers are
  free-form strings** — the familiar vocabulary is convention only, so kenward must not
  validate against it.
- lore's SQLite runs WAL with a single connection and a 5s busy timeout, so concurrent
  calls contend. Client concurrency is bounded and busy errors are retried.

## 13. Non-goals

Not built, and not designed around: billing, tenant orchestration, a web dashboard, a
control plane, SSO, organisations and teams, usage quotas, automatic memory writing
without confirmation, and any multi-tenant runtime. One container per household is what
makes all of these bolt-on rather than structural.
