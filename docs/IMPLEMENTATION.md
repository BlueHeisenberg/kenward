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
cmd/kenward/            entrypoint: run, setup, invite, revoke, doctor, update, version
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
| `golang.org/x/sys` | v0.47.0 | `setup` (terminal echo suppression) |
| `github.com/BlueHeisenberg/keel` | v0.4.0 | `routing` (llm), `session` (vault), `supervisor` (sandbox), `updater` (update) |

Note: the MCP SDK requires Go 1.25, which is why the module targets it.

Adding a dependency outside this table requires a decision recorded in
`ARCHITECTURE.md`, not a `go get`.

---

## 2. Domain types — `internal/domain`

```go
type MemberID string   // stable internal id; NOT the Telegram id
type SpaceID string    // lore space identifier, always passed explicitly

type Member struct {
    ID          MemberID
    Name        string
    TelegramID  int64     // 0 until claimed
    Private     SpaceID
    Tiers       []string  // ordered tier chain for this member's private conversations
    BotTokenEnv string    // env var name; isolated mode only, empty in simple mode
    EnrolledAt  time.Time
}

type Household struct {
    Name        string
    Shared      SpaceID
    GroupChatID int64
    Tiers       []string // ordered tier chain for the group conversation
}

type ScopeKind int
const (
    ScopeUnknown ScopeKind = iota  // the zero value; never valid for a resolved Scope
    ScopeDirect
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
    Confidence string    // lore's vocabulary; validated inside internal/memory
    Markers    []string
    Origin     string
    CreatedAt  time.Time
    UpdatedAt  time.Time
    Partial    bool      // this is a search excerpt, not a whole entry
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

`Confidence` and `Origin` carry lore's vocabularies and are checked as the client parses
lore's output, inside `internal/memory`. `internal/config` has no say in it: the
vocabularies belong to lore, not to a household's configuration, and nothing in
`kenward.yaml` may widen or narrow them.

`Partial` is the one field a caller may not infer. lore's search returns a snippet — a
body that may be elided in the middle, with no origin and no timestamps — while `Get`
returns the whole entry, and only the client knows which it just handled. It is a field
rather than a heuristic because the consequence of getting it wrong is invisible:
anything rendering an entry into a prompt must say which it has, and a model shown a
fragment under a heading claiming completeness will answer confidently from the part it
can see. `PROMPT.md` describes how the prompt says so; an invariant test asserts that
every entry crossing the `Memory` interface carries the right value.

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
    Name    string
    BaseURL string
    Model   string
    // No credential field. See "An endpoint carries no credential" below.
    Tags    []string  // tier names
    Timeout time.Duration
}

type Message struct {
    Role    string // system | user | assistant | tool
    Content string
}

type ToolSpec struct {
    Name        string
    Description string
    Schema      json.RawMessage
}

type ToolCall struct {
    ID        string
    Name      string
    Arguments json.RawMessage   // raw on purpose; malformed args are the caller's to reject
}

type Request struct {
    Messages    []Message
    MaxTokens   int
    Temperature *float64          // pointer so that 0 — deterministic — can be expressed
    Tools       []ToolSpec
}

const (
    FinishStop          = "stop"
    FinishLength        = "length"
    FinishToolCalls     = "tool_calls"
    FinishContentFilter = "content_filter"
)

type Completion struct {
    Text         string
    ToolCalls    []ToolCall
    FinishReason string
    Endpoint     string
    Tier         string
    Latency      time.Duration
}

// Router is routing policy: which machine answers, given the chain.
type Router interface {
    Complete(ctx context.Context, chain []string, req Request) (Completion, error)
}

// Completer is the wire protocol against one endpoint. Pool decides who to ask,
// a Completer does the asking, tests inject fakes.
type Completer interface {
    Complete(ctx context.Context, ep Endpoint, req Request) (Completion, error)
}

// KeyFunc resolves one endpoint's API key at the moment of use. Empty value with
// a nil error means the endpoint needs no authentication.
type KeyFunc func(ep Endpoint) (string, error)

func NewPool(endpoints []Endpoint, c Completer) *Pool   // *Pool implements Router
func NewHTTPCompleter(client *http.Client, key KeyFunc) Completer

// ErrNoBackend carries what was tried, so the refusal can be specific.
type NoBackendError struct{ Chain []string; Tried []string }
func (e *NoBackendError) Error() string
```

**An endpoint carries no credential.** `routing.Endpoint` has no `APIKeyEnv` — the
configuration type of the same name still does, being one of the sources — and no key
field of any other name, because a key may come from a file, an environment variable or a systemd
credential (§4), and a struct field naming one of the three is misinformation the moment
an operator chooses another. Precedence across those sources — and the rule that naming
two sources for one secret is a validation error — is implemented once, in
`internal/config`; routing never learns where a key lives.

**Routing resolves at the point of use and retains nothing.** The completer is
constructed with a `KeyFunc` and calls it per attempt, so a rotated credential is picked
up without rebuilding the router, and the value goes straight into the `Authorization`
header. It is stored on no struct this package keeps, so no log line, error string or
`%#v` can meet it. A key that will not resolve is a configuration fault: it is returned
to the caller as-is and never triggers failover, because trying a different machine
cannot conjure a key.

Anyone auditing where this household's keys live should read `internal/config`, not this
package. Routing is the wrong place to look, and that is the design.

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

Keys are unwrapped into memory only, never written to disk, and zeroed on `Lock`.
`LockAll` runs on shutdown signals.

**Idle expiry is off by default** (`session.idle_timeout`, `session.DefaultIdleTimeout`
is zero). That follows from D-019: a passphrase never travels over Telegram, so a member
whose key is zeroed has no way to unlock again from a chat — someone has to be at the
machine to start the process with the passphrase. A default timeout would therefore not
degrade an idle member, it would break them, in exchange for a marginal at-rest gain
while the process is still running anyway. What holds either way is the claim that was
ever true: nothing is readable from a disk, from a backup, or from a process nobody has
unlocked.

Setting a positive `idle_timeout` turns expiry on and it works as written — a household
that wants it can have it, knowing an expired member stays silent until someone attends
to the machine. The isolated-mode privacy statement in `internal/privacy` is worded to
be true in both configurations, and its golden test pins that; the two must not drift
apart again.

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

data_dir: /var/lib/kenward    # where kenward's own mutable state lives; empty means
                              # the per-OS default from config.DefaultDataDir()

household:
  name: "Home"
  shared_space: household
  group_chat_id: -1001234567890
  tiers: [local, local-slow, cloud]

telegram:
  # simple mode: one token for everything. bot_token_file is the alternative;
  # naming both is an error. With neither, the systemd credential `bot_token`.
  bot_token_env: KENWARD_BOT_TOKEN
  # isolated mode: per-member tokens, see members[].bot_token_env

members:
  - id: david
    name: David
    telegram_id: 12345678
    private_space: david-private
    tiers: [local]            # local-only: refuses rather than reaching for cloud
    bot_token_env: KENWARD_BOT_TOKEN_DAVID   # isolated mode only
    # bot_token_file: /run/secrets/david-token   # or this; never both

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
    # api_key_file: /run/secrets/openrouter    # or this; never both
    tags: [cloud]

memory:
  lore_command: ["lore", "mcp"]
  search_limit: 8

session:
  idle_timeout: 0s            # off, and the default; see §"Idle expiry is off by default"

capture:
  max_proposals_per_turn: 1

update:
  channel: stable             # stable | edge | off
  check_interval: 6h
```

`data_dir` holds only what kenward writes about itself — today `state.json`, the record
of which Telegram account is bound to which member. It is not where lore keeps its data
and it never holds a secret. Left empty it resolves to the per-OS state location; the
`--data-dir` flag and `$KENWARD_DATA_DIR` override it in that order, which is how the
container image runs with no arguments. See `CLI.md`.

Validation rules that are errors, not warnings:

- every tier named in `household.tiers` or `members[].tiers` must be a tag on at least
  one endpoint
- `private_space` values must be unique across members and must not equal
  `household.shared_space`
- `telegram_id` values must be unique
- in `isolated` mode every member must have a distinct bot token source: two members
  naming the same `bot_token_env`, or the same `bot_token_file`, share a bot and defeat
  the mode
- naming two sources for one secret is an error, not a precedence — see below
- every secret the configuration depends on must be readable at startup, or `kenward`
  exits with a list of what is missing and where each one was looked for; it never
  starts half-configured

### Where a secret comes from

**The configuration names secrets. It never holds one.** That has always been true; what
changed is that an environment variable is no longer the only way to name one, because
it is the wrong instrument in the two deployments that matter most. In a pod, the
environment is readable by every process in the container and visible in `/proc`, and a
member's pod holds that member's bot token — whoever holds a bot token reads every
message ever sent to it. Under systemd, an `EnvironmentFile=` value sits in the
process's environment for as long as it runs and is inherited by every child it spawns,
including the `lore mcp` subprocess, which has no business seeing a Telegram token.

So a secret has three possible sources:

| Source | Form | Applies to |
| --- | --- | --- |
| A file | `bot_token_file`, `members[].bot_token_file`, `endpoints[].api_key_file` | any deployment that can mount a file |
| An environment variable | `bot_token_env`, `members[].bot_token_env`, `endpoints[].api_key_env` | a hand-run binary, and the compose path |
| A systemd credential | no configuration at all | the systemd unit |

Resolution is `_file` → `_env` → credential.

**Naming both `_file` and `_env` for the same secret is a validation error, not a
precedence win.** This is the rule most likely to be read as pedantry, so the reasoning
is worth stating: a precedence order would let the configuration keep working while one
of the two lines is dead, and the operator goes on believing the value comes from
somewhere it does not. That belief is the whole problem — it is the state in which a
rotated file changes nothing, a revoked variable is never noticed, and the eventual
debugging session starts from a false premise. Two sources means somebody holds a belief
about this secret that is not true, and interrupting them costs one error message.

The automatic credential does not collide with anything, because it is not a *stated*
source: it is the fallback consulted only when the configuration says nothing.

**Files.** The value is the file's contents with trailing line endings trimmed — every
trailing `\r` and `\n`, and nothing else. Every tool that writes a credential adds one, a
CRLF editor adds two bytes, `printf '%s\n\n'` adds more, and a token carrying any of them
is rejected by Telegram with an error that names nothing useful. Interior whitespace is
left alone, because a secret with a space in the middle is a secret rather than a
mistake, and no credential may legitimately end in a newline; a file that is nothing but
newlines is reported empty rather than resolved to `""`. A file that is group- or
world-readable is **refused**, with its mode in the message: a `0644` token file is a
finding rather than a preference, since everything with a shell on that host then holds
the household's bot, and failing loudly is the only way the operator who created it ever
learns. The check is skipped on Windows, and that is stated rather than hidden:
`os.Stat` there synthesises permission bits from the read-only attribute alone, with no
relation to the ACL that actually governs access, so enforcing would refuse every file on
the platform while proving nothing about any of them.

**systemd credentials.** If `$CREDENTIALS_DIRECTORY` is set, kenward looks in it, with no
configuration whatsoever, for:

```
bot_token                 the household bot (simple mode, and the group unit)
bot_token.<member id>     a member's own bot
api_key.<endpoint name>   one per endpoint that needs a key
```

The unit file already names each credential; making the operator repeat the name in
`kenward.yaml` would only create a second place for the two to disagree. Names that
systemd would not accept — anything outside letters, digits, `_`, `-` and `.` — disable
the automatic lookup for that member or endpoint rather than becoming a path.

Two properties of this mechanism are surprising enough to be worth writing down, because
each one is a trap in the opposite direction:

- **The source file never needs chowning.** systemd reads the path on the `LoadCredential=`
  line *as PID 1*, before it drops to the unit's user. So the file in
  `/etc/kenward/credentials/` stays `root:root` mode `0600` for good, even under
  `DynamicUser=`, where the uid is different on every start and could not be chowned to
  in advance. The re-ownership happens automatically, and only on the copy systemd hands
  to the unit.
- **`$CREDENTIALS_DIRECTORY` is ephemeral and is not `StateDirectory`.** It is a
  non-swappable tmpfs created for one unit and torn down when the unit stops. Nothing
  written there survives a restart, and nothing about it is a place to keep state.
  Expecting otherwise is the mistake to warn an operator against.

`deploy/kenward.service` uses `LoadCredential=` for exactly these reasons.
`deploy/README.md` explains why the compose files stay on environment variables: there
is no portable Compose primitive equivalent to a credential, so they scope secrets as
tightly as per-service `environment:` blocks allow and point at the `_file` form for an
operator who wants the stronger guarantee.

A resolved value is never stored on the `Config` and never appears in a message. It is
handed back on demand by an accessor, behind a closure rather than a string field, so
that no amount of `%+v` on a configuration can print a token; the type's `String()` says
where the value came from and not what it is. Names, paths, variable names and file modes
appear freely — that is what makes a fault fixable.

**Only the resolver is wired.** `config.Secrets` implements all three sources and
validation uses it, so a configuration naming a file or relying on a credential
validates, and `kenward run` will not start if that file is unreadable. But the
components that actually *use* a token still read the environment variable name directly
— `internal/supervisor` when it hands a bot token to a unit or a pod, `doctor` when it
reports whether tokens are set, and the update health check when it asks whether Telegram
authorises. None of them holds a `*Secrets`. The consequence is specific and worth
knowing before an operator meets it: a household supplying its token only through
`bot_token_file` or a systemd credential passes validation and then finds no token where
it is needed. Until the accessors are plumbed through the supervisor, `bot_token_env` and
`api_key_env` remain the sources that work end to end, and `deploy/kenward.service`'s
`LoadCredential=` lines are ahead of the runtime rather than behind it.

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
   explicit refusal naming the tiers tried — never a silent fallback. Any other router
   failure becomes one of the notices in §10; a turn never ends in silence.
6. **Reply.**
7. **Capture.** If the model proposed a memory write, run the capture state machine.
8. **Record** the turn in the unit-local history ring.

History is unit-local, in memory, bounded (default 20 turns), and is **not** written to
lore. lore holds distilled knowledge, not transcripts.

---

## 6. Capture

Model proposals arrive as a structured tool call: `{title, body, domain, confidence,
markers, target: personal|shared|unsure}` on the `remember` tool.

A second tool, `publish`, carries the promotion flow and is offered in a direct
conversation only. It takes `{title}` and no id — see *Promotion* below.

| Situation | Behaviour |
| --- | --- |
| Direct chat, target `unsure` | Ask: `[Personal] [Household] [Don't save]` |
| Direct chat, target known | Ask to confirm: `[Save to X] [Don't save]` |
| Group chat, any target | Ask: `[Household] [Don't save]` — **"Personal" is never offered** |
| Promotion of an existing private entry to shared | Separate flow, triggered by the `publish` tool: show the full text that will be published, then `[Publish to household] [Cancel]` |

Rules:

- At most `capture.max_proposals_per_turn` (default 1) proposals per turn.
- A proposal whose title matches one declined in the last 10 turns is suppressed.
- Timeout on the question is treated as **declined**, never as accepted.
- The answer is only accepted from `AllowedUserID`.
- Promotion uses `memory.Share`, never a read-then-put, so lore's own provenance is
  preserved.

**Promotion.** It is the one memory act a member asks for rather than being offered, so
it needs a trigger, and the trigger is the `publish` tool. The member asks; the model
calls `publish` with the *title* of an entry, never an id.

The absence of an id is the point. lore's ids are global and `lore_get` is not
space-scoped, so an id is a capability: whoever holds one can name an entry in any
space (§12). An id may only originate from a search performed inside the current Scope,
and the model is not such a source — everything it writes derives from what the member
just said, so an id from the model is an id from the member. The node therefore
resolves the title against **this turn's own retrieval**, in the space the Scope writes
to, and uses the id that search returned. A title matching no retrieved entry, or more
than one, is dropped with a log line exactly as a malformed `remember` is: nothing is
asked, and nothing reaches memory — not even the `Get` behind the preview. A group
scope is never offered the tool, and `OfferPromotion` refuses one anyway.

Promotions are neither counted against the per-turn proposal budget nor suppressed by
the decline history: they are a deliberate act the member asked for, not a suggestion.
When one turn carries both a `publish` call and a `remember` proposal, the publish wins
— the request outranks the suggestion, and exactly one question reaches the member
either way.
- **A write that fails after the member said yes is reported to them, not just logged.**
  lore may have stored the entry and lost the answer, and lore has no delete, so a retry
  that duplicates it is permanent (§12). The member is told that it cannot be confirmed
  either way and to check before saving it again, and the title is suppressed for the
  next ten turns so the model does not immediately re-propose the thing they were just
  asked to verify.

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

`internal/updater` owns kenward's side of that: when checks happen, and what health,
drain and consent mean for a household. keel owns the mechanism and states the full
threat model in its own package documentation; this package defers to it entirely. The
single authority that can put new code on a household's machine is an Ed25519 release
signature verified against keys compiled into this binary — never the update host, never
the network path to it, never the scheduler. Everything past signature verification is
availability engineering, and nothing in it may prevent kenward from starting or serving:
a scheduler that cannot be constructed is a warning and a household that does not
auto-update, never a refusal to start.

### Consent is three-valued, and unanswered is the zero value

`keel/update`'s `Consent` hook takes a request carrying the version pair, the notes, and
the `SecuritySensitive` flag, so the question can say *which* of the two reasons it is
being asked. That distinction is not cosmetic: "there is a major version" and "this
release changes behaviour that decides whether a private conversation may reach a
provider" deserve different amounts of thought, and collapsing them into one sentence
hides the one that matters.

The answer is `Approved`, `Declined` or `Unanswered`, and **`Unanswered` is the zero
value on purpose.** A declined release is remembered and not raised again until a
different version appears — somebody decided. An unanswered one is asked about again on
the next cycle, because a timed-out question, an undeliverable message or a pipe that
ended is not a decision. Getting this the other way round would mean one unheard question
suppressing a security release forever, which is the failure this design exists to avoid.
Neither outcome applies the update; the distinction controls only whether the household
is asked again.

At a terminal, agreeing is typing `y` and refusing is typing `n`. Anything else —
including no stdin at all — is `Unanswered`, and it says so.

### What of that is wired

The requirements above are the destination. Most of them are now reached; two are not,
and the distance is worth stating precisely, because a household reading this section
would otherwise conclude that every release arrives on its own.

**Wired.** `kenward run` builds an `updater.Scheduler` and runs it alongside the
supervisor. It checks on `update.check_interval`, verifies signatures, refuses a manifest
older than `DefaultMaxManifestAge`, honours the stable channel's delay, drains, swaps,
health-checks and rolls back. Health is the real check in both the scheduled path and
`kenward update` — lore answers and *this process's own* bot token authorises, which in a
member's pod means that member's token rather than the household's, since checking a
credential the process does not hold would fail health on a perfectly good pod. Drain is
the supervisor's own drain rather than a second mechanism: the supervisor is the one
component that knows whether a turn is in flight, and two sources of that truth would
eventually disagree, at which point a restart lands in the middle of somebody's
conversation. A restart request stops intake and lets the serve loop finish its own
shutdown, and it is issued after a *failed* swap too — a household that has been drained
and then left running would be silently ignoring its members, which is worse than one
that restarts.

**Not wired: consent over Telegram.** The scheduler's `Consent` hook is deliberately nil,
which means the scheduled path never applies a major version or a release flagged
`securitySensitive` — it logs the refusal once per version and leaves it, while patch and
minor releases keep applying. That is the safe failure and it is what this document
promises, but it is not the promise fulfilled: a household on an old major version is
told nothing.

It is nil for a structural reason rather than an unfinished one. The question has to
reach the household over kenward's own transport, and this layer cannot get at it: the
supervisor owns the bot and does not expose it, and Telegram long-polling admits one
consumer per token, so a second transport built here would either never see the member's
tap or would steal updates from the units and break the assistant in order to ask a
question about updating it. `internal/updater` defines the `Consenter` interface the
answer will implement — a member's tap, taps from anyone else not counting, timeout and
undeliverable both meaning `Unanswered`. Wiring it means the supervisor exposing a way to
ask the household group something, and that is the supervisor's decision.

**Not wired: one member at a time.** This one is not merely unbuilt; the obvious
implementation does not exist and cannot. keel's cross-process lock serialises processes
that share **one install path** — several pods running off one mounted binary — and it is
taken *before* the drain, so a process that loses it skips the cycle quietly without
having silenced anybody first. But containerised isolated-mode pods each carry a private
copy of the binary in their own image filesystem. They never contend for that lock, so it
delivers no per-member sequencing for them at all. Two independent analyses reached the
same conclusion: rolling one member at a time is not expressible from inside the process
being updated. It belongs to whatever owns the pods' shared artifact — the image, and the
process that rolls it — and pretending otherwise would be a promise the lock cannot keep.

`INSTALL.md` and `CLI.md` describe the update path in these terms rather than the
requirements list's.

---

## 10. Refusals

Refusal text is a product surface. It must say what happened and why, and never imply a
capability that does not exist:

> No machine in your allowed tiers (`local`) is reachable right now — `monster` and
> `5090` were unavailable. This conversation is limited to that tier, so I won't send it
> anywhere else. Wake one of them and ask again.

Every clause is chosen. "Your allowed tiers" becomes "the household's allowed tiers" in
a group conversation, because the chain being enforced is the group's. "Were
unavailable" is preferred to "I tried", because `NoBackendError.Tried` lists endpoints
that were attempted alongside endpoints skipped for a cooldown or a failed probe, and
claiming an attempt that never happened is a small untruth in a message whose whole
value is being accurate. And the refusal names the boundary rather than the
configuration behind it: a member is told this conversation goes no further, not that
somebody set their space to local-only.

Refusals are golden-tested, in `internal/assistant/testdata`. Changing one is a
deliberate edit to a test fixture.

### Every turn produces a reply

A refusal is only one of the ways a turn can fail to produce an answer, and silence is
the one response that teaches a household the assistant is broken and unpredictable. A
message that arrives always produces something, and the node writes it: a model that
cannot be reached cannot explain why it cannot be reached.

Beyond the tier-chain refusal, the router's other failures are classified into three
notices, chosen so that each one tells the member the truth about what they can do next:

| What happened | What is sent |
| --- | --- |
| Rate limited (HTTP 429) | "The model is busy right now. Try again in a moment." |
| A rejected key, an unknown model, a request the endpoint will not parse (400, 401, 403, 404, invalid request) | "Something is wrong with this household's setup — tell whoever runs it." |
| Anything else | "Something went wrong reaching the model, and your message wasn't answered. Try again in a moment." |

The middle row is the one that earns its place: no amount of retrying fixes a rejected
key, the member cannot repair it, and the operator can — so the notice sends them to the
person who can. **400 belongs in that row, not in the transient one.** It is the endpoint
saying it will not parse this request — the same permanent fault as `ErrInvalidRequest`,
caught one hop later — and telling a member to try again for it sends them back to a
wall.

Three further notices come from outside the router. A model that declined the turn
produces "The model declined to answer this."; a direct message arriving while the
member's key is locked produces "Your assistant is locked. It needs to be unlocked on the
machine it runs on."; and a turn that ran to the end and produced nothing the member
could see produces "I didn't get a usable answer to that. Try asking again."

That last one covers the two ways a turn can succeed and still say nothing: a completion
with no text and no tool call, and a bare tool call whose capture proposal was suppressed
without asking. Neither is a failure the node can classify further, and neither may be
answered with silence — this section promises every message produces something, and until
this notice existed that promise had two paths where it was untrue.

All of them are golden-tested alongside the refusals.

The classification reads `keel/llm`'s error vocabulary, which the routing seam passes
through unchanged; a content-filter decline commonly arrives as an empty response
carrying the finish reason rather than as a completion with text.

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
- **`lore_get` and `lore_share` are not space-scoped.** Entry ids are global, so an id is
  effectively a capability to read an entry in any space. The client verifies the fetched
  entry's space and returns `ErrNotFound` on a mismatch, but that verification resolves
  spaces by display name and lore does not enforce uniqueness on names. The primary
  defence is therefore upstream: **an entry id must never be taken from member-supplied
  text.** Ids originate from a search performed within the current Scope, or from a
  promotion flow that already resolved one.
- **`Search` returns a snippet, not a body**, with no origin and no timestamps, and
  `CreatedAt` can never be populated because lore's MCP surface exposes it on no tool.
  Prompt rendering must not imply it holds a full entry when it holds a snippet.
- **A write whose answer is lost is never replayed.** A `lore_put` may have landed even
  though the client reported failure, and since lore has no delete, a retry that
  duplicates it is permanent. Report uncertainty to the member rather than silently
  retrying.
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
