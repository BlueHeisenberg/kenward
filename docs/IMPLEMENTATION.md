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
  (`scope.ErrNotEnrolled`, `session.ErrLocked`, `capture.ErrMemberNotified`, …). Wrap
  with `%w`. Where the caller needs the *details* of the failure and not just its
  identity, the error is a struct rather than a sentinel — `routing.NoBackendError`
  carries the tier chain and the endpoints tried, because the refusal shown to a member
  names them.
- **Every package has table-driven tests.** Unit tests make no network calls and touch
  no real Telegram, provider or lore instance — all three are interfaces with fakes.
- **Nothing writes to memory without an explicit user confirmation.** No exceptions, no
  config flag to disable the confirmation.

---

## 1. Package tree

```
cmd/kenward/            entrypoint: run, setup, invite, revoke, doctor, update, version
cmd/kenward-release/    release tooling: signing keys, manifests, sign, verify.
                        Never shipped in the image or the release artifacts.
internal/domain/        core types. Depends on nothing.
internal/config/        YAML load, defaults, validation
internal/scope/         message -> Scope resolution. THE authorization boundary.
internal/memory/        Memory interface + lore MCP client
internal/transport/     Transport interface + Telegram implementation + Mux
  .../telegramtest/     a loopback Bot API server for tests. Ships in the module
                        rather than under _test.go because internal/e2e uses it.
internal/routing/       tier chain, endpoint pool, probe, cooldown, failover
internal/session/       key unwrap, idle expiry
internal/capture/       capture proposal + confirmation state machine
internal/enrol/         claim codes, onboarding state machine
internal/assistant/     the per-member Unit: one turn, end to end
internal/supervisor/    simple (goroutines) | isolated (pods)
internal/updater/       update checks, health gating, rollback, consent
internal/privacy/       the per-mode privacy statements, stated once
internal/setup/         first-run wizard
internal/version/       build metadata
internal/e2e/           tests only, no production code: whole messages through
                        the real wiring, faked only at the three outer edges
```

Dependency direction is strictly downward: `domain` depends on nothing; `assistant`
depends on the interfaces, never on their implementations. `supervisor` wires
concrete implementations together and is the only package that may import all of them.

One exception, and it is deliberate rather than an erosion: `assistant` imports
`keel/llm` for its **error vocabulary** alone, to classify a router failure into the right
member-facing notice (§10). It reaches no provider and constructs no client; the routing
seam passes those error types through unchanged, so re-declaring them in `routing` would
only add a mapping that could disagree with itself.

Third-party dependencies, fixed:

| Module | Version | Used by |
| --- | --- | --- |
| `github.com/go-telegram/bot` | v1.23.0 | `transport` |
| `github.com/modelcontextprotocol/go-sdk` | v1.7.0 | `memory` |
| `gopkg.in/yaml.v3` | v3.0.1 | `config` |
| `golang.org/x/sys` | v0.47.0 | `setup` (terminal echo suppression) |
| `github.com/BlueHeisenberg/keel` | v0.5.4 | `routing` and `assistant` (llm), `session` (vault), `supervisor` (sandbox), `updater`, `cmd/kenward` and `cmd/kenward-release` (update) |

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

**A reply is split; a question is not.** Telegram caps one text message at 4096 UTF-16
units, so `Send` splits an over-long `Outbound` across several messages and delivers them
in order. `Ask` refuses instead, with `ErrTextTooLong`: the buttons belong to exactly one
message, and a question split across two would put the choices under half of what they
answer. Splitting is the right answer for prose and the wrong one for a decision.

The transport's other sentinels are the malformed-input guards on the same boundary:
`ErrUpdatesActive` (a second `Updates` stream on one transport), `ErrEmptyText` (a message
or question carrying nothing), and `ErrNoChoices` (a question with no buttons — a question
nobody can answer).

`Mux` fans a single bot's `Updates` channel out to scoped `Transport` views, each defined
by a predicate over `Inbound` rather than by a key: a member's view matches
`!in.IsGroup && in.UserID == <their telegram id>`, and the household group's matches
`in.IsGroup && in.ChatID == <the group chat>`. A member is therefore identified by who
sent the message and the group by which chat it arrived in — never the other way round,
which is what keeps a member's direct view from ever seeing a group message. Simple mode
uses the Mux; Isolated mode does not (each pod owns its own bot).

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
func NewHTTPCompleter(client *http.Client, key KeyFunc, logger *slog.Logger) Completer

// NoBackendError carries what was tried, so the refusal can be specific. It is a
// typed error, not a sentinel: the caller needs the chain and the endpoint names.
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

**Not every failure is a machine failure.** An endpoint error is cooled and routed
around only when trying a different machine could plausibly help: a transport failure,
or an HTTP 5xx. A 4xx would be rejected identically everywhere. And an **empty response**
is three cases, not one:

| Empty response | Fails over? | Cooled? | Why |
| --- | --- | --- | --- |
| No choices, or an empty choice with nothing else | yes | yes | The endpoint said nothing. It is broken, exactly like a 5xx. |
| `finish_reason` = `content_filter` | no | no | The model declined. A refusal is a final answer; walking the chain would offer the same content to every machine in turn, and for a chain ending in a cloud tier that hands a provider content a local model just declined. |
| `llm.EmptyResponseError.Reasoning` non-empty (`Detail` = `"reasoning only"`) | no | no | The model spent the turn thinking and produced no answer. The endpoint worked and the model worked; the next machine running the same class of model thinks itself into the same silence. What helps is room to answer in, not a different machine. |

Both of the non-failover cases return the `*llm.EmptyResponseError` to the caller
unchanged, so `internal/assistant` can tell the member what actually happened (§10).

**The finish reason cannot identify a reasoning-only turn, and must not be asked to.**
Measured against the household's own vLLM endpoint, the identical request came back with
null content under both `length` and `stop`, the latter with a third of the token budget
unspent. The discriminator is the typed `Reasoning` field — never a substring match on
`Detail`.

**Failover happens before the first token only.** Once a response has begun, an error
is returned to the caller as a partial failure; there is no retry, because retrying
produces spliced or duplicated output.

**Cooldown:** an endpoint whose failure justified failover is cooled for 30s, doubling
per consecutive failure to a 5m ceiling, reset on success. A decline and a reasoning-only
turn are not failures and do not cool anything.

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

The passphrase itself outlives startup, in the closure that provisions a mid-run claim
(§7). The buffer read from the credential file is still zeroed the moment startup ends;
what persists is the string the KDF was given, which was never reclaimable on demand
anyway. That is a real cost and the alternative was worse: without it, enrolment cannot
finish while the node runs. It changes little in practice, because this process is
already holding every key that passphrase wraps, unwrapped, for its whole life — D-019
says so plainly — so a memory dump learns the passphrase rather than anything the
passphrase was protecting here.

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
  # A lore space id from `lore spaces`, never a display name. See below.
  shared_space: 7d5047bb-d939-4539-b3db-8b6221a2e245
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
    private_space: dac31e70-72e4-4b10-9cef-a6276c4a87b8   # a space id, not a name
    tiers: [local]            # local-only: refuses rather than reaching for cloud
    bot_token_env: KENWARD_BOT_TOKEN_DAVID   # isolated mode only
    # bot_token_file: /run/secrets/david-token   # or this; never both
    passphrase_env: KENWARD_PASSPHRASE_DAVID # isolated mode only; wraps david's key
    # passphrase_file: /run/secrets/david-passphrase   # or this; never both

endpoints:
  - name: monster
    base_url: http://monster.tail:8000/v1
    model: qwen3.6-27b-awq
    tags: [local]
    timeout: 120s
    context_window: 262144        # what the server was started with, not the model card
    max_completion_tokens: 32768  # a reasoning model thinks inside this budget
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    model: anthropic/claude-sonnet-5
    api_key_env: OPENROUTER_API_KEY
    # api_key_file: /run/secrets/openrouter    # or this; never both
    tags: [cloud]
    context_window: 200000        # defaults to 16384; see §5 on the budget
    max_completion_tokens: 8192   # defaults to 4096; must be < context_window

memory:
  lore_command: ["lore", "mcp"]
  search_limit: 8
  announce_reads: true        # prefix each reply with what was searched; default true

session:
  idle_timeout: 0s            # off, and the default; see §"Idle expiry is off by default"
  # simple mode only, and optional: the one node passphrase that wraps every member's
  # key. Name it and its absence is reported at load, by name; leave both out and the
  # systemd credential, KENWARD_PASSPHRASE and the terminal prompt still apply.
  passphrase_env: KENWARD_PASSPHRASE
  # passphrase_file: /run/secrets/kenward-passphrase   # or this; never both

capture:
  max_proposals_per_turn: 1
  private_writes: save        # save | ask; see §6 "Writing then telling"

update:
  channel: stable             # stable | edge | off
  check_interval: 6h
```

`data_dir` holds only what kenward writes about itself, and is not where lore keeps its
data. Left empty it resolves to the per-OS state location; the `--data-dir` flag and
`$KENWARD_DATA_DIR` override it in that order, which is how the container image runs with
no arguments. See `CLI.md`.

A handful of files live there, and **two of them are secret material** — which is the fact
that decides how the directory is backed up, mounted and permissioned:

| File | Contents | Sensitive |
| --- | --- | --- |
| `state.json` | the enrolment bindings: which Telegram account is which member | no |
| `sessions.json` | each member's **wrapped key** | yes |
| `invites.json` | the **hashed** claim codes not yet redeemed | yes |
| `invites/<id>.json` | isolated mode only: one member's outstanding codes, for their pod | yes |
| `revocations/<id>.json` | isolated mode only: that one member was revoked, and when | no |

The per-member files under `invites/` are derived from `invites.json` by `kenward invite`,
and exist because the process that redeems a code in isolated mode is not the process that
minted it — see §8, "How a supervisor-started pod gets a claim code". One file per member,
never the whole store, because a member's pod may hold nothing of anybody else's.

`revocations/` is the same arrangement for the same reason in the other direction, written
by `kenward revoke`: the process holding a binding in isolated mode is not the one asked to
clear it. A record is a member id and a timestamp — nothing to withhold, which is why it
is the one file here that is world-readable, so the compose deployment can mount it into a
container that runs as a different account. See §8, "How a revocation reaches the pod that
holds the binding".

Neither sensitive file holds a plaintext secret: a wrapped key needs the passphrase and a
claim code is stored only as a digest. But a wrapped key is worth exactly one offline
guessing campaign to whoever copies it, which is why `sessions.json` is written `0600`
inside a `0700` directory, and why this directory is not a thing to sync somewhere
convenient. A backup of `data_dir` is a backup of every member's key material.

### Spaces are configured by id, never by display name

`shared_space` and `private_space` hold lore **space ids** — the id column of `lore
spaces`, not the name a human gave the space. `internal/memory` keys every call on the
id and resolves the display name from it, and this is not a preference. lore does not
enforce unique display names, and the rule that an entry id must never come from
member-supplied text (§12) rests on the client being able to check which space an entry
actually lives in. That check is worth nothing if two spaces can answer to one name.

The failure mode is what makes it worth a heading rather than a footnote. lore's own
tools accept either form, and its `lore_put` argument is lenient, so a display name here
**writes successfully and fails on the first read**. A household would configure it,
capture memories for a week, and discover on the first retrieval that nothing comes back
— with the writes already permanent: they were made under a name, and a delete needs the
id from a receipt nobody read. `kenward doctor` therefore
treats a name it cannot resolve to a space as a configuration fault and exits 2, telling
the operator to run `lore spaces` and take the id column, and saying explicitly that a
name fails only on reads.

Validation rules that are errors, not warnings:

- every tier named in `household.tiers` or `members[].tiers` must be a tag on at least
  one endpoint
- `private_space` values must be unique across members and must not equal
  `household.shared_space`
- `telegram_id` values must be unique
- in `isolated` mode every member must have a distinct bot token source: two members
  naming the same `bot_token_env`, or the same `bot_token_file`, share a bot and defeat
  the mode
- in `isolated` mode every member must have a passphrase source — `passphrase_env`,
  `passphrase_file`, or the systemd credential `passphrase.<id>` — and it must be
  distinct per member for the same reason: one passphrase across two members is one
  wrapping secret for both their keys, which is simple mode's custody with isolated
  mode's name on it. It is required for a member who has not claimed yet as much as for
  one who has, because the claim is what provisions their key and it happens in their
  own pod (D-023) with nobody at a terminal to be asked
- naming two sources for one secret is an error, not a precedence — see below
- every secret **this process's own unit** depends on must be readable at startup, or
  `kenward` exits with a list of what is missing and where each one was looked for; it
  never starts half-configured

### Secrets are demanded of the unit, not of the household

Everything above is a property of the file and is checked in full by every process that
reads it. The last rule is not: which secrets have to *resolve* depends on which unit the
process was asked to be, because in isolated mode a pod is given the household's whole
configuration and only its own unit's secrets. That is D-007 — a pod that held a
sibling's bot token would be a pod that could read that member's private conversation —
and it is what both compose files and the host supervisor already do. Demanding the
household's secrets in a pod would have meant putting every member's token in every
member's container, which is exactly the failure the mode exists to prevent.

`config.UnitScope` carries the selection; its zero value is the whole household, so a
simple-mode node and an operator at a terminal are unchanged. `Config.ValidateForUnit`
and `Config.MissingSecretNamesForUnit` take it. There are three kinds of process:

| Process | Bot tokens it must have | Passphrases it must have | Provider keys it must have |
| --- | --- | --- | --- |
| Household node (zero scope) | the household's, plus every member's in isolated mode | every member's, in isolated mode: it is the host supervisor, and it provisions each pod's | every endpoint's |
| The group's pod (`--group` / `KENWARD_GROUP=1`) | the household's, when `group_chat_id` is set | **none** — it serves the shared space and holds no member's key | the endpoints `household.tiers` reaches |
| A member's pod (`--member ID` / `KENWARD_MEMBER=ID`) | that member's, and no other — not even the household's | that member's, and no other | the endpoints that member's `tiers` reaches |

Provider keys are scoped by tier chain for the same reason `deploy/compose.isolated.yml`
hands each container only the keys its own chain can reach: a key present in an
environment is a key that can be used whatever routing intended. A key that is *named*
and unset is a hard fault rather than an absent optional secret, so an unscoped check
refused a local-only member's pod for obeying its own configuration.

What the scope does **not** touch: mode, endpoints, tier chains, space uniqueness,
`telegram_id` uniqueness, bot-token-source uniqueness, the lore command, the limits, the
update channel, and the "one source per secret" rule. Every pod reads the same file, so a
file that cannot be served is refused by whichever pod picks it up. One check is added
rather than relaxed: a process told to serve a member the file does not name is refused,
because a scope that selects nobody would otherwise select no secrets and report itself
healthy.

**There is a fourth row, and it has no unit at all.** `kenward invite` mints a digest and
`kenward revoke` clears a binding; neither opens a bot, unwraps a key or calls a provider,
so both load the file with every secret unresolved (`UnitScope.NoSecrets`). Unscoped —
which is what they were — they demanded *every* member's bot token and passphrase, and in
isolated mode no container holds a sibling's, which is the mode rather than an oversight.
The result was that no service could run either command, on a host that has no container
of its own to run them in, and `deploy/README.md` documented an operator command that was
unrunnable by construction. What is dropped is resolution and nothing else: every
structural rule above still applies, "one source per secret" included, because that one is
a property of the file rather than of the environment; and a command naming somebody the
file does not declare is still refused, by the command itself, which has to find them
before it can mint or revoke for them anyway. `run` and `doctor` never set it — they are
about to serve, and a node that starts half-configured is the failure resolution exists to
prevent.

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
| A file | `bot_token_file`, `members[].bot_token_file`, `members[].passphrase_file`, `session.passphrase_file`, `endpoints[].api_key_file` | any deployment that can mount a file |
| An environment variable | `bot_token_env`, `members[].bot_token_env`, `members[].passphrase_env`, `session.passphrase_env`, `endpoints[].api_key_env` | a hand-run binary, and the compose path |
| A systemd credential | no configuration at all | the systemd unit |

`members[].passphrase_*` is the isolated-mode session passphrase, and it is a secret the
configuration *names* like any other — the invariant is untouched, no value goes in the
file.

`session.passphrase_*` is simple mode's, where one node passphrase wraps everybody's key.
It is **optional**, and that is the one asymmetry in this table. A node passphrase has
three deliveries the file cannot see — `$CREDENTIALS_DIRECTORY/kenward-passphrase`,
`KENWARD_PASSPHRASE`, and a terminal prompt, tried in that order — and every one of them
is a legitimate way to run simple mode, so a file that names no source is not a fault and
all three still work exactly as before. What naming a source buys is the thing its
absence cost: a *stated* variable that is not set, or a *stated* path that cannot be read,
is reported at load with the name in it, the way `members[].passphrase_env` already was
in isolated mode. Unnamed, a missing node passphrase is discovered further in, where a
container has nothing left to do but exit 2 and be restarted forever —
`deploy/compose.simple.yml` shipped in exactly that state.

The corollary is worth stating because it is the trap: **name a source only if that is
really how the passphrase arrives.** A named variable the deployment does not set is a
refusal, so the systemd unit — which delivers this secret by
`LoadCredential=kenward-passphrase` — must leave both fields out, and `kenward setup`
writes neither for the same reason. When a source is named, it wins over all three
fallbacks; a pod in isolated mode uses its own member's and never this.

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
passphrase.<member id>    the passphrase wrapping that member's key (isolated mode)
api_key.<endpoint name>   one per endpoint that needs a key
kenward-passphrase        the node passphrase (simple mode)
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

**All three sources work end to end.** `config.Secrets` implements them, validation uses
it, and so does every component that actually consumes a secret: both supervisors and the
single-unit runner resolve bot tokens and endpoint keys through it
(`supervisor/simple.go`, `supervisor/single.go`, `supervisor/isolated.go`,
`endpointKeyFunc` in `supervisor/runner.go`), `doctor` reports each secret by the
configuration path it was named on and by where it was looked for, and the update health
check resolves the token it needs to ask whether Telegram authorises. A household
supplying its token only through `bot_token_file` or a systemd credential is a supported
household, and `deploy/kenward.service`'s `LoadCredential=` lines describe the runtime
rather than run ahead of it.

Nothing holds a resolved value. Each consumer holds a `func() (config.Secret, error)` and
calls it at the moment of use, which is what makes rotation work: a token file or
credential rewritten under a running node is picked up the next time the closure is
called, with no restart to reload a value that was never cached.

**Into a pod, delivery mirrors the stated source.** `Isolated` resolves each pod's secrets
*on the host* and provisions them so that the pod's own resolver — reading the same
configuration — finds each one exactly where the host did: an environment variable stays an
environment variable, a `*_file` becomes a `0600` file at the same path, and a
systemd credential becomes the same credential name under a synthetic
`CREDENTIALS_DIRECTORY` at `/run/kenward/credentials`. Values are added to the pod spec
only at Create and Recreate time, so none rests on a struct a logger could print, and
a rotated source is picked up by the next recreation without a supervisor restart.

Each pod gets exactly the secrets its unit needs and no others: a member's pod gets that
member's bot token and that member's passphrase; the group's pod gets the household bot
token and no passphrase at all.

Every provisioned secret file is **owned by the identity the image runs as** — uid=gid=65532,
the distroless base's `nonroot`. That is not a nicety. keel provisions a file root-owned
unless told otherwise, and a root-owned `0600` file is one the pod's own process cannot
open, so before it was set the file and credential sources failed in this deployment path
while the environment one worked — a pod refusing its own configuration with `permission
denied`. Loosening the mode is not the alternative: `config.Secrets` refuses a secret file
that is group- or world-readable, so `0644` trades one refusal for another and hands the
token to every process in the container on the way past.

One constraint follows from that and is worth knowing before it is met: in isolated mode a
`bot_token_file` or `passphrase_file` **must be an absolute path**, because a relative one
names nothing determinate inside the pod. It is refused with that reason rather than
provisioned to a guess.

**What the host learns, stated plainly**, because it is the property the mode exists to
provide. The host process holds no member's wrapped key, no member's lore instance and no
member's plaintext — those live in the pods' work volumes and the pods' memory. What it
does do, at Create and Recreate time and only there, is read a member's passphrase in
order to hand it to that member's pod, and drop it again. There is no way around that: a
supervisor that starts a process with a secret holds that secret for the length of the
call, and `keel/sandbox` has no bind-mount by which a host could pass a path instead of a
value. So the claim is not "the host never sees a member's secret" — that would be false —
but three narrower ones that are true, and that simple mode keeps none of:

- the host retains nothing; every value is resolved through a closure at the moment of use
  and never cached (the same mechanism that makes rotation work);
- no member's secret is ever given to another member's pod, or to the group's;
- what the host forwards is a wrapping secret for a key it does not have.

An operator with root on the household machine can of course reach both halves — they run
the container runtime. Isolated mode has never claimed otherwise (D-021 accepts a
root-writable key record; D-019 accepts that a key stays unwrapped in its pod's memory).
What it claims, and what this delivery keeps true, is that no *component* of kenward holds
another member's plaintext, and that one member's compromise reaches no one else.

---

## 5. The turn

`internal/assistant.Unit.Handle(ctx, in Inbound) error`:

1. **Resolve scope.** `scope.Resolve(cfg, in)`. Unknown sender or unmapped chat →
   return `ErrNotEnrolled`; the caller drops it silently, sending nothing. Never reply
   "you are not authorised" — that confirms the bot exists to a stranger.
2. **Ensure session.** Only a member has a key, so a group turn proceeds without one. If
   the member's key is not unwrapped, the turn stops with the locked notice from §10 and
   nothing else: retrieval without a session would be retrieval without the member
   present. The notice deliberately does **not** invite the member to send a passphrase.
   There is no unlock flow over Telegram and there cannot be one — a passphrase typed into
   a chat travels through Telegram, stays in the member's own history, and in simple mode
   is readable by whoever holds the bot token. Otherwise the session's idle clock is
   touched and the turn continues.
3. **Retrieve.** The member's message is reduced to its content words — lore's own
   tokens, minus a stopword list, capped at six — and each word is searched on its own
   in every space in `scope.Read`, concurrently. Each space's hits are unioned and
   ranked by what each word narrowed down — a word contributes `1/(entries it found)`
   to each of them, so an entry every word found still outranks one a single word
   brushed, and a precise word outranks two ordinary ones. Ranking by a plain count of
   words instead loses the answer: the per-word budget is the same `memory.search_limit`
   the union is truncated to, so two everyday words fill every slot before a precise
   word is looked at, and the tie breaks against the one entry that answers the
   question. Results stay grouped in scope order and are never re-ranked across spaces.
   Budget: `memory.search_limit` per space.

   The message is **not** the query, and that is not a refinement. lore's search is a
   conjunctive full-text match over bare words: no stemming, no operators, no prefixes,
   and every term must be present. "what is the boiler service code?" therefore
   retrieves nothing from a household that recorded exactly that sentence, because
   "what" is not in it — and the node then answers as though nothing had been stored.
   Searching word by word makes retrieval degrade instead of failing outright: one
   relevant word among six filler ones still finds the entry.
4. **Assemble.** System prompt + retrieved entries (rendered with their markers and
   confidence) + the last N turns from the unit-local history ring, trimmed to fit
   `Options.ContextBudget` with `Options.MaxTokens` reserved out of it for the completion.
   `MaxTokens >= ContextBudget` is a construction error, not a runtime surprise: it leaves
   no room for a prompt at all.

   The budget is the **smallest** context window among the endpoints in this unit's tier
   chain, supplied by the wiring, because the Unit cannot know which endpoint will answer
   — the router picks one *after* the prompt is assembled. Assembling for the largest and
   failing over to the smallest would be a truncation nobody chose. The cost is stated
   rather than hidden: mixing endpoints with materially different windows inside one tier
   wastes the larger ones.

   Both numbers come from the endpoints. `endpoints[].context_window` and
   `endpoints[].max_completion_tokens` are per-endpoint because they are facts about a
   machine and the model it serves, not household policy, and `Config.ChainLimits` reduces
   them to one pair per tier chain by taking the minimum of each. The completion cap is a
   minimum for the same reason the window is: the turn may land on any endpoint in the
   chain.

   The two minima cannot contradict each other, and that is load-bearing rather than
   lucky. Validation requires every endpoint's `max_completion_tokens` to be **smaller
   than its own** `context_window`, reported at load with the endpoint named; the endpoint
   holding the smallest window contributes a cap no larger than its own, so the derived
   cap is always below the derived budget and `assistant.New`'s
   `MaxTokens >= ContextBudget` check can never fire on derived numbers. That check remains
   for values a caller sets directly.

   `DefaultContextWindow` is 16384 and `DefaultMaxCompletionTokens` is 4096, applied per
   endpoint by `ApplyDefaults`; `assistant.DefaultContextBudget` and
   `assistant.DefaultMaxTokens` are those same two constants, so the assistant's fallback
   and the configuration's default cannot drift apart. The window default is deliberately
   modest rather than generous: it is the figure used for an endpoint nobody described, and
   guessing high overflows a small server mid-conversation — a provider error in front of a
   member — where guessing low only wastes a window somebody bought. The completion default
   is deliberately not tight: a reasoning model spends that budget on hidden tokens before
   emitting any content, and a cap sized for a plain instruct model makes it return a full
   reasoning trace, no content, and `finish_reason: stop` — which reaches the member as the
   §10 "no usable answer" notice from a model that is working perfectly.
5. **Route.** `router.Complete(ctx, scope.Tiers, req)`. A `*NoBackendError` becomes an
   explicit refusal naming the tiers tried — never a silent fallback. Any other router
   failure becomes one of the notices in §10; a turn never ends in silence.
6. **Reply**, prefixed with the retrieval line.
7. **Capture.** If the model proposed a memory write, run the capture state machine.
8. **Record** the turn in the unit-local history ring — the reply alone, without the
   retrieval line.

### The member is told what was read

The reply carries a line naming the memories that were searched and how many entries
reached the answer:

```
[searched your private memory (2 entries), the household memory (nothing)]

The boiler was serviced in March.
```

Three decisions are in that line, and each was the other way at some point.

**It rides on the reply rather than arriving as its own message.** A turn already costs
a reply and may cost a write announcement; a third message on every single turn — most
of them reporting that nothing was found — is how a household learns to stop reading any
of them, at which point the announcements that matter are lost with the noise. It is
prefixed exactly as the degraded-retrieval line would be, and the net message count of a
turn did not rise: a private write now costs one message where it used to cost two.

**It counts what reached the model, not what retrieval found.** The budget loop drops
entries from the end of a group to make the prompt fit, and a line claiming five entries
informed an answer the model saw two of would be a statement about the answer's basis
rather than about retrieval. `assemble` therefore returns the prompt input as the loop
left it, and the line is counted off that.

**It is silent when nothing was searched.** A message with no content words — a greeting,
an emoji — produces no search terms and therefore no searches, and the groups come back
empty for that reason rather than because the spaces are. Reporting "(nothing)" about a
search that never ran is exactly the class of small untruth §10 refuses elsewhere. A
space whose search *failed* says `(couldn't be read)` for the same reason the prompt says
so to the model.

It is not recorded in history. It is the node accounting for itself, not the assistant's
words, and a line fed back as an assistant turn teaches the model to write those lines
itself — at which point a member cannot tell a real accounting from a fabricated one.

`memory.announce_reads` turns it off; it defaults to on. That is a setting where the
write announcement in §6 is not, and the asymmetry is the point: a read changes nothing,
so a household that finds the line noisy loses nothing but the line.

History is unit-local, in memory, bounded (default 20 turns), and is **not** written to
lore. lore holds distilled knowledge, not transcripts.

### Turns are serialised

A `Unit` runs **at most one turn at a time**. The history ring, the capture engine's
decline window and its per-turn proposal budget are all per-unit state that a second
concurrent turn would interleave with the first, and the resulting conversation would be
one nobody had. Serialising is cheaper than making each of those safe to interleave, and
it is also what a member expects: a reply belongs to the message before it.

Scope is resolved *before* admission (step 1 above), so nothing below can send anything to
a stranger.

A message arriving mid-turn joins a bounded queue and waits for the turn slot:

| Knob | Default | Meaning |
| --- | --- | --- |
| `Options.QueueLimit` | `DefaultQueueLimit` = 8 | how many messages may wait behind a running turn |
| `Options.QueueNoticeAfter` | `DefaultQueueNoticeAfter` = 2s | how long a queued message waits in silence first |

- Under the limit, the message waits. If it is still waiting after `QueueNoticeAfter`, the
  member is told **once** that it is queued — the queue notice in §10 — and then it keeps
  waiting. Once, not repeatedly: a member who has been told is told, and a second notice
  is noise about noise.
- At the limit, the message is **dropped with a notice**, not silently. Dropping silently
  and being ignored are indistinguishable from inside a chat, and one of them is a bug the
  household would report as the other.
- A cancelled context ends the wait; nothing is sent.

Both defaults are small on purpose. A queue deep enough to hide a stuck turn is worse than
a short one that admits to being full, because the member finds out either way and only
one of the two tells them in time to do something about it.

Two boundaries of the mechanism are worth stating, because both are easy to assume the
other way:

- **The turn slot covers steps 3–8 and stops there. It does not cover the capture
  question.** That question waits on the *member*, not on the node, so it is run after the
  slot is released. Holding the slot across it would mean a member who ignores the buttons
  and asks something else gets a queue notice blaming them for a turn that is waiting on
  their own tap — or, at the limit, gets their next message dropped by a "busy" node that
  is doing nothing at all.
- **Order among queued messages is not guaranteed.** They contend for the slot rather than
  forming a line. Telegram's own delivery makes no ordering promise either, so a queue
  that invented one would be pretending to a property the layer beneath it does not have.
  What *is* guaranteed is the part that matters: no message is processed twice, and no
  unit state is touched outside the running turn.

---

## 6. Capture

Model proposals arrive as a structured tool call: `{title, body, domain, confidence,
markers, target: personal|shared|unsure}` on the `remember` tool.

A second tool, `publish`, carries the promotion flow and is offered in a direct
conversation only. It takes `{title}` and no id — see *Promotion* below.

| Situation | Behaviour |
| --- | --- |
| Direct chat, target `personal` | **Write it, then announce it**: the full draft, the space it went to, and `[Undo]` — see *Writing then telling* below |
| Direct chat, target `personal`, `capture.private_writes: ask` | Ask to confirm: `[Save to personal] [Don't save]` |
| Direct chat, target `shared` | Ask to confirm: `[Save to household] [Don't save]` |
| Direct chat, target `unsure` | Ask: `[Personal] [Household] [Don't save]` |
| Group chat, any target | Ask: `[Household] [Don't save]` — **"Personal" is never offered**, so nothing in the group is ever written unasked |
| Promotion of an existing private entry to shared | Separate flow, triggered by the `publish` tool: show the full text that will be published, then `[Publish to household] [Cancel]` |

### Writing then telling

A note in a member's own private space is theirs and it is reversible, so it is written
and then reported. A publication to the household is not reversible — other people have
read it by the time anyone regrets it — so it is asked about first, every time.

What is configurable, and what deliberately is not:

| | Configurable | Why |
| --- | --- | --- |
| Private write: save or ask | **Yes**, `capture.private_writes` | A household that preferred the question gets it back, unchanged. Defaults to `save`. |
| Shared write: always asks | **No** | Publishing to the household is the one irreversible act in the product. |
| The write is announced | **No** | If a write can be silent, *"kenward never records anything without telling you"* stops being true and there is no honest way left to describe the product. |
| The read is announced | **Yes**, `memory.announce_reads`, default on | A read changes nothing. |

The announcement carries the **whole draft**, title and body, not a summary of it. Under
the old flow the member read the exact words before the write and could refuse them; this
message is now the only place those words are ever shown, so an announcement carrying a
title alone would quietly turn *"kenward tells you what it wrote"* into *"kenward tells
you that it wrote"*.

The order is load-bearing. The write is real before the member is told about it, so the
announcement is a report rather than a promise, and an ignored announcement leaves the
entry exactly where the message says it is.

### Undo

`[Undo]` is live for `Options.AskTimeout` — the same window a capture question waits, and
deliberately not a second knob. Every ending is a different sentence, because the entry is
in a different state in each:

| Ending | What the member gets |
| --- | --- |
| Delete confirmed, or the entry was already a tombstone | *"Removed … It won't come back in an answer, here or on any other device in the household."* Outcome is `OutcomeUndone`, and `Stored()` is false. |
| lore refused the delete | *"I couldn't take that back: … is still in your private memory."* The outcome stays `OutcomeSaved`, because it is. |
| lore never answered (`ErrWriteUncertain`) | *"I can't tell whether … was removed — the memory store didn't answer."* Both "removed" and "still there" would be guesses about the member's own memory. |
| Nobody taps; the window closes | The announcement is edited in place, keeping its text and appending *"— the undo window has closed; this is still in memory"*. |
| The announcement itself could not be sent | A plain confirmation, saying the undo button did not go through. Silence here would be a silent write. |

Reporting a failed or unconfirmed delete as "undone" would be the plainest lie the product
could tell: the member asked for something to stop existing and would be told it had.

The success sentence is bounded to what lore actually does. A delete is a signed tombstone
that propagates, not a shred — the entry stops coming back from search and get, and the row
is still on the disk — so the message promises that it will not be recalled rather than
that it was erased. ARCHITECTURE.md required the announcement to say which of the two it
is, and this is that sentence.

**An undo is recorded as a decline.** Without that it achieves nothing that lasts — the
same conversation produces the same proposal next turn and the default policy writes it
straight back. The old flow got this for free, because a decline *was* a tap.

**A second tap cannot reach the delete.** The transport retires the announcement on the
first one — keyboard stripped, pending question forgotten — and later taps on a keyboard
still on somebody's screen are dropped silently like any stale tap. lore's own idempotence
(a second delete reports already-deleted rather than erroring) is the second line, not the
first.

The retirement wording is the caller's, through `transport.Question.RetiredNote`. The
default line says the question was declined, which is right for a question and wrong for a
message reporting a write: *"I've written this to your private memory … — no answer,
treated as declined"* reads as the write having been called off, and a member who believes
it will not go looking for an entry that is really there.

Rules:

- At most `capture.max_proposals_per_turn` (default 1) proposals per turn, **per
  speaker**.
- A proposal whose title matches one **that speaker** declined in the last 10 turns is
  suppressed.
- Both budgets key on the member who spoke, not on the conversation, and in the household
  group that distinction is the whole point: one member's question is not another
  member's, so N speakers in one turn window may each be asked once, and a title member A
  declined is still offered to member B. Keying on the conversation instead would let the
  first speaker in a group spend everyone's budget, and let one member silently decide
  what another is never asked about.
- A proposal that never became a question — the question could not be built, or could not
  be sent — **refunds** the budget it took, on the same reasoning as duplicate
  suppression: only a question the member actually saw should cost them the one they were
  allowed.
- Timeout on a question is treated as **declined**, never as accepted. Timeout on a write
  announcement is the undo window closing on a write that stands.
- The answer is only accepted from `AllowedUserID` — on the undo button as much as on a
  question, and with more at stake there: a stranger's tap would delete out of somebody
  else's private memory.
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
- **A write whose outcome is unknown is reported to the member, not just logged.** lore
  may have stored the entry and lost the answer. lore *can* delete, but only by id, and
  the id was in the receipt that never arrived — so there is nothing to clean up with and
  a retry that duplicates it is permanent (§12). The member is told that it cannot be
  confirmed either way and to check before saving it again, and the title is suppressed
  for the next ten turns so the model does not immediately re-propose the thing they were
  just asked to verify. This holds on both paths: the write is the same write whether a
  tap authorised it or the policy did.

---

## 7. Enrolment

Telegram bot usernames are publicly discoverable and anyone may `/start`. Therefore:

1. Operator runs `kenward invite --name "David"` → prints a single-use claim code with
   an expiry (default 24h). Codes are stored hashed. In isolated mode the digest is also
   written to that member's own file for their pod to be given, because the process that
   redeems it is not this one — §8, "How a supervisor-started pod gets a claim code".
2. A stranger messaging the bot gets **no reply at all** until a valid code is
   presented. Not an error, not a prompt — silence.
3. On a valid code: bind `telegram_id` → member, mark the code consumed, provision and
   unlock that member's key, and run the short onboarding explaining the two memories
   and how capture works. **The private space is not created here.** Nothing in kenward
   creates a lore space, anywhere; the space named in `private_space` must already
   exist, and enrolment binds a person to a configuration entry rather than
   provisioning storage.
4. Codes are single-use, expiring, rate-limited (5 attempts per chat per hour) and
   compared in constant time.

**A claim mid-run completes without a restart, key included.** Keys were once
provisioned and unlocked at startup only, so somebody who claimed while the node was
running got their unit and their onboarding and then "Your assistant is locked" on
their first private message — a notice whose only remedy is an operator restarting the
node, on the first thing a household ever does with kenward, addressed to the one
person who cannot perform it. The process that binds a claim is the process that will
serve that member, and it holds the passphrase that wraps their key: in simple mode the
operator's node passphrase, which by that mode's definition wraps everyone's; inside a
pod that member's own. So it provisions and unlocks there and then
(`supervisor.UnlockOnEnrol`).

The order is a safety property, not a detail. Whether this process serves the member is
decided **first**; only then is a key touched. A claim that lands on a pod's bot for
somebody another pod serves is bound and left there — no key, no unit — because a
member's key provisioned inside another member's pod would be wrapped under the wrong
passphrase, which is the one thing isolated mode exists to prevent.

Removal is `kenward revoke <member>`; it unbinds the Telegram id and reports that the
space key must be rotated in lore. It takes effect at the next start in either mode — a
running node decided who it serves when it started — and in isolated mode it cannot
perform the unbinding at all, only record it for the pod that holds the binding. Both are
said in its output, and §8's "How a revocation reaches the pod that holds the binding" is
why. It refuses outright while `kenward.yaml` declares that member's `telegram_id`, which
is the operator's line to delete.

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

There is a **third implementation, and it is not a third mode**: `Single`, selected by
`kenward run --member ID` or `--group`. It serves exactly one unit and is what runs
*inside* an isolated pod — `isolated` is the host half of that mode, `Single` the pod
half. It is listed here because §8 is where a reader goes to learn what process runs
where, and the pod's own process is otherwise invisible.

`Single` differs from `simple` in the two ways the pod boundary forces, and in no others:
it fixes session custody to isolated mode over a store that holds exactly one wrapped key,
and it builds its transport from *that member's* `bot_token_env` rather than the
household's. It also has a state `simple` has no need for: a member whose pod exists but
who has not yet claimed their invite starts **claim-only** — no unit, no session, nothing
touching lore, just a claimer waiting for the code. A member's bot exists before they
claim and the claim happens in a conversation with that bot, so the pod must be able to
serve the claim without being able to serve a turn. When the claim binds, the member's
key is provisioned and unlocked under **that member's own passphrase**, named by
`members[].passphrase_env` / `_file` (or the systemd credential `passphrase.<id>`) and
delivered to that pod alone, and the unit starts serving in place, with no restart — see
§7 and "How a supervisor-started pod gets a passphrase" below.

**Every member in the configuration gets a pod, claimed or not**, and that is D-023
rather than a convenience: the operator adds the member, starts their pod, and hands
over a code the member redeems against their own bot. `Isolated` therefore builds the
same pod for an unenrolled member as for an enrolled one — same argv, same configuration,
that member's own token and their own passphrase, both of which §4 already requires of
them for this reason. It used to skip them (*"supervisor: member not enrolled, no pod"*),
which left D-023's sequence reachable through the compose path — a service per member
regardless — and through a hand-run process, and unreachable through `kenward run`: the
one deployment path that manages itself was the one that could not onboard anybody.

The two facts an operator needs are kept apart in `Health`. A **running** pod whose
member has not claimed reports `StateNotEnrolled`, "awaiting enrolment" — the same answer
the pod's own process gives about itself — because calling it ready would say a member is
being served when nobody has arrived. A claim-only pod that will not start is
`StateFailed`, with its error and its restart count, exactly like any other. Before this
the two were indistinguishable: a pod that was never created and a pod that could not
start both reported `StateNotEnrolled` with a nil error, because the not-enrolled record
was virtual and `fail` skipped it. What the host still cannot see is a claim that lands
*after* it started — the code is redeemed inside the pod against the pod's own invite
store on its own volume, so the host goes on reporting "awaiting enrolment" for a member
who is by then being served, until the next `kenward run` re-reads the configuration. That
volume is also why the code has to be *carried* into the pod rather than looked up there;
see the next-but-two heading.

### How a supervisor-started pod gets lore

The image deliberately carries no `lore` (the Dockerfile says so in its first paragraph:
lore is a sibling project with its own release cadence, and baking a copy in would pin
kenward to whatever version was current at image build time). It names two remedies —
bind-mount a `lore` binary at `/usr/local/bin/lore`, or build a derived image that
`COPY`s one there — and the two deployment paths do **not** have the same choice between
them:

- **The compose path** takes the bind-mount. `deploy/compose.isolated.yml` carries
  `./bin/lore:/usr/local/bin/lore:ro,z` on every service. The `z` is the SELinux relabel
  every bind mount in both compose files now carries — without it an enforcing host
  (Fedora, RHEL, CentOS Stream) refuses the container read access to the host file and
  the pod exits on a permission error naming no cause. Shared files take `z`, the
  per-member invite and revocation files take `Z`, and the header of each file says why
  swapping them breaks a household; neither option does anything on a host that is not
  enforcing.
- **The supervisor path can only use the derived image.** `sandbox.Spec` has `Image`,
  `Env`, `Command` and `Files` and no host bind-mount, so there is nothing for
  `Isolated` to mount with. The operator builds `FROM ghcr.io/blueheisenberg/kenward:<tag>`
  with a `COPY` of a `lore` binary for the image's own OS and architecture, and passes it
  as `kenward run --image`. That is the whole answer, and it needs saying because the
  *default* is the published image: `--image` omitted starts pods from
  `ghcr.io/blueheisenberg/kenward:<this host's version>`, which by design has no lore in
  it.

`Spec.Files` could physically carry the binary — its `Mode` is permission bits, so 0755
is expressible, and keel provisions through a tar stream that preserves it — and it is
still the wrong tool: it would copy tens of megabytes into every pod on every create and
every rolling update, to reproduce what a three-line Containerfile does once. Adding a
bind-mount to keel would be a domain-free mechanism keel could reasonably own, but it is
a cross-repo API change to buy an operator convenience the derived image already
provides. Neither is proposed.

**Without lore, `run` refuses to start, and that is the load-bearing part.** Nothing
downstream fails on a missing lore: `memory.NewClient` checks only that the command is
non-empty and spawns nothing until the first call, and a turn that cannot read a space
degrades that space rather than failing (§5). So a pod with no lore came up, reported
itself ready — the supervisor observes the container, not the unit — authorised its bot,
and then remembered nothing anyone told it, with the only trace a "could not be read"
line inside a prompt. `run` now settles the question before it builds anything, and exits
1 naming the remedy. The check belongs there rather than in validation because whether
lore works is a property of the machine, not of the file — a validation that failed on
one host would make `doctor` useless for checking a configuration before shipping it
(§4).

**The question is "does lore answer", not "is lore installed", and the difference is
where this was wrong first.** `exec.LookPath` on `memory.lore_command[0]` was the whole
of the check, and it stops one step short of the failure it exists to prevent: `lore mcp`
exits *before* the MCP handshake when `LORE_HOME` holds no account —

```
lore: load account (run `lore init` first?): no account at /home/nonroot/.lore/account.json (run `lore init`)
```

— which is the state every fresh container volume is in. Measured against a real
container, the binary was on `$PATH`, the check passed, and the node started into exactly
the silence the check exists to prevent. So `run` now does both: the PATH lookup, whose
refusal names the two ways to get a binary into an image, and then one bounded MCP
handshake through the same seam `doctor` probes with, so the two cannot drift. Its
refusal names `lore init`.

Two lines are drawn deliberately. **Only lore failing to answer is fatal** — a space lore
does not hold is one space's problem and `doctor`'s to report, and refusing a household
its assistant over a mistyped space id would be a larger outage than the silent one being
prevented. And the handshake is a **hard** refusal, bounded at thirty seconds, rather than
a warning with a retry: `lore mcp` is a local subprocess over a local SQLite store, there
is no network in this path, and its one transient failure — store contention — is already
retried with backoff inside `internal/memory`. Whatever has not answered by then is not
momentary. A node that hangs at startup is worse than one that refuses: a container
runtime restarts it either way, and only the refusal leaves a line an operator can read.

The one process exempt is the **isolated host supervisor**, and not as a concession: it
starts pods and holds no memory client, no transport and no key. Each pod spawns its own
`lore mcp` over its own `LORE_HOME` and asks the question of its own image on its own way
up. Demanding lore of the host too refuses every correctly-configured isolated household
on a machine whose lore lives only where it is used — which is exactly what the first
real-podman run of this check did, refusing before a single pod was started.

The `Unit` implementation is identical in all three. If a change to `Unit` needs to know
which one it is running under, that change is wrong.

### How a supervisor-started pod is configured

D-022's two isolated deployment paths hand a pod the same things by different means, and
the pod's own process cannot tell which one started it. Compose mounts `kenward.yaml` at
`/etc/kenward/kenward.yaml` and passes `--config … --data-dir … --member ID | --group` in
`command:`. The host supervisor provisions the same file at the same path
(`supervisor.PodConfigPath`) from `IsolatedOptions.ConfigFile` — which `kenward run` fills
with whichever configuration it is itself serving — and starts the pod with that same
argv.

The argv is not decoration. Without it a pod runs the image's own `CMD`, which names
`--config /etc/kenward/kenward.yaml --data-dir /var/lib/kenward`: the first against a path
this deployment path would then never provision, and the second beating the
`KENWARD_DATA_DIR` the supervisor sets, which would put the member's wrapped key somewhere
other than `/work` — the only volume `Recreate` preserves — so the first rolling update
would take it.

**The whole household file goes into every pod, not a per-member slice of it.**
Configuration names secrets and never holds one (§4), and `Isolated` provisions only that
pod's own token beside it, so what a member's pod gains is the household roster and the
*names* of other members' token variables and file paths, never a value: no lore space, no
chat and no bot becomes reachable from holding it. Filtering it would buy nothing and
would put the two deployment paths back out of step, which is what D-022 decided against.

The consequence is that the pod must read a household file while holding one unit's
secrets, so everything it checks is checked at the scope of its own unit — see
"Secrets are demanded of the unit, not of the household" in §4. That applies past
validation: `kenward doctor` authorises only this unit's bot token, probes only this
unit's lore spaces (the shared space, plus this member's private one), and reports key
custody and tier notes for this unit alone. It has to, because the image's `HEALTHCHECK`
runs `doctor` — as a *second* process with no arguments of its own, so it takes the unit
from `KENWARD_MEMBER` / `KENWARD_GROUP` in the container's environment. The host
supervisor already sets those; `deploy/compose.isolated.yml` sets them alongside the
`--member` in `command:` for exactly this reason, and the two must agree or `kenward`
refuses to start (D-022). Without it a member's pod would be marked unhealthy for every
sibling secret it correctly does not hold, and restarted every thirty seconds forever.

The file is read once when the supervisor is constructed and every pod is recreated from
that snapshot, so a configuration edited while the household is running reaches pods on
the next `kenward run` rather than on the next `Roll`.

### How a supervisor-started pod gets a passphrase

This is where the mode failed outright the first time it was run against a real container
runtime, so it is written down rather than left to be inferred. `readPassphrase` accepted
a systemd credential, `KENWARD_PASSPHRASE`, or a terminal prompt. A pod has no terminal,
the host provisioned neither of the other two, and `kenward.yaml` had nowhere to name one.
Every member's pod exited at startup with *"no session passphrase available, so no
member's key can be unwrapped"*, on every restart, forever.

A passphrase cannot arrive any other way. D-019 settles that it never travels over
Telegram, and there is no second channel to a member: the operator supplies it when the
process starts or it does not arrive. So it is a per-member secret named in the household
configuration beside that member's bot token, and delivered by the same mechanism:

- **The supervisor path.** `Isolated` resolves `members[].passphrase_*` on the host and
  provisions it into that member's pod, mirroring the stated source — see §4's "Into a pod,
  delivery mirrors the stated source", which also states plainly what the host learns by
  doing so. A member whose passphrase cannot be resolved stops the whole household from
  starting, next to the same refusal for an unresolvable bot token: one pod that silently
  never comes up is worse than a household that refuses and says why.
- **The compose path.** There is no host, so the operator sets the same variable on that
  member's service and on no other, or mounts the file the member's `passphrase_file`
  names. `deploy/compose.isolated.yml` does the first, with the reasoning in its header.

**One passphrase per member, never one for the household.** Isolated mode's whole session
custody is that each member's key is wrapped under their own — a household-wide passphrase
would mean one compromised container's secret opens every member's memory, which is simple
mode's custody with isolated mode's name on it, and nothing downstream would report it:
every pod would unlock and every member would be served. Two members naming the same
`passphrase_env` or `passphrase_file` is therefore a validation error, in the same
sentence as two members naming the same bot token.

**The group's pod gets none.** It serves the shared space and holds no member's key, so a
passphrase there would unwrap nothing — and a secret that opens a member's memory, sitting
in the one pod every member talks to, is exactly what the mode exists to keep out of it.

**And simple mode had the same hole, one layer up.** Isolated mode's passphrase is a
configuration field, so a pod handed none is refused at load with the variable named.
Simple mode's node passphrase was not a field at all, so nothing validated its absence:
`deploy/compose.simple.yml` shipped without one and the container restart-looped on exit 2
forever, with the refusal listing three mechanisms and naming no variable, because there
was no variable in the file to name. `session.passphrase_env` / `_file` closes it — see §4
for why the field is optional, why naming a source the deployment does not supply is worse
than naming none, and why `kenward setup` writes neither.

### How a supervisor-started pod gets a claim code

This is the last step of D-023 and it did not work, found the same way the passphrase was:
by running the mode against a real container runtime. `kenward invite` mints into
`<data_dir>/invites.json` **on the host**. The pod's claimer reads its own store under its
own `--data-dir`, which is `/work/kenward` — the pod's named volume — and nothing crossed
between the two. So the operator ran `kenward invite --name Jordan`, was handed a code,
handed it on, and jordan's pod, which by then exists and is waiting claim-only, had no
record of it and refused it. Correctly and in silence, because that is what enrolment owes
a sender it does not recognise (§7), which is exactly why nothing anywhere reported it:
every command succeeded and the member was simply never enrolled.

Four shapes of fix were weighed, and the choice turns on what the host is thereby able to
do to a member's volume, because keeping the host out of that volume is the whole of what
this mode sells.

- **Provision the code into the pod at create time**, the way the configuration, the bot
  token and the passphrase already travel. Chosen.
- **`kenward invite` writes through into the running pod's store.** Fresher, and rejected.
  It needs the host to write inside a running member's container, and every mechanism that
  can do that (`podman cp`, `WriteFile`, an exec) is one small edit from reading the same
  volume back out. A member's volume holds their wrapped key and their lore instance, and
  isolated mode exists so the host does not reach into it. Rejected on that, not on cost.
- **The pod mints its own code and the host reads it back.** Cleaner ownership and worse
  in both other respects: it needs `Exec` into a member's container, which is a strictly
  larger capability than placing one file into a fresh one, and it does not map onto the
  compose path at all, where there is no host process to ask.
- **Share one invite file between host and pods.** A non-starter: `enrol.FileStore` is
  atomic against other operations in its own process and explicitly not against a second
  process on the same path, and here there would be one writer per pod.

So: `kenward invite` derives a per-member file, `<data_dir>/invites/<id>.json`, and both
deployment paths deliver it to `/etc/kenward/invites.json` inside that member's pod —
`Isolated` provisions it through `Spec.Files` at create time, compose bind-mounts it
read-only. The pod is told where it is with `--invites` and **imports** it into its own
store on the way up.

What crosses, stated exactly, because a mechanism that moves enrolment material between
trust domains is worth being precise about:

- **Hashed, never plaintext.** A record is a PBKDF2-HMAC-SHA256 digest of an 80-bit
  `crypto/rand` code at 210,000 iterations. The plaintext exists once, in the operator's
  terminal (§7).
- **One member's, never the household's.** `invites.json` holds every member's digests;
  the file a pod is given holds one member's. Digests, so the exposure would have been
  theoretical — the rule is not.
- **Into the container, never into the volume.** `/etc/kenward` is the container's own
  filesystem, replaced on every recreation. `/work` is the member's, and nothing on this
  path writes there or reads from it. The host gains no new access to a member's memory
  and the direction is one-way.
- **The pod's consumed marks are never overwritten.** Redemption happens in the pod and
  the host's copy never learns of it, so the import skips digests the pod already holds
  rather than replacing them. Overwriting would restore a spent single-use code to
  redeemable on the next rolling update.
- **The group's pod gets none, and is not even told to look.** D-023 puts the claim on the
  member's own bot, so the household's pod has no claimer; `PodCommand` omits `--invites`
  for it.

**The cost, which is real: the seed is a snapshot taken when the pod is created.** A code
minted while that member's container is already running does not reach it until the
container is next created — which `Start` now makes a restart do, for the pods that could
be holding a stale copy; see "How a revocation reaches the pod that holds the binding"
below, where the same instruction turned out to be false and both halves were fixed
together — the same staleness §8 already documents for the host's view of
enrolment, and for the same reason: `Isolated` reads the configuration once and pods are
recreated from that snapshot. It costs nothing in the flow D-023 actually describes, since
adding a member means editing `kenward.yaml` and its secrets and therefore restarting the
node anyway, and a claim-only pod is serving nobody, so recreating it interrupts nothing.
It does cost something when re-minting for a member whose pod is already up, so `kenward
invite` says so in isolated mode rather than leaving it to be discovered. The alternative —
the host pushing into a running member's container — is the option rejected above, and this
staleness is the price of not having it.

### How a revocation reaches the pod that holds the binding

The same crossing, and the harder half of it, found immediately after the one above and
initially left alone. `kenward revoke` unbinds in the **host's** `state.json`. In isolated
mode the binding is not there: it was written by the pod when the claim was redeemed, into
the state file on that pod's own volume. So revoking a member emptied a record no pod ever
reads, printed *"messages from that Telegram account are ignored from now on"*, exited 0,
and the pod carried on serving them. That is worse than the invite defect it mirrors —
that one failed where somebody could see it, and this succeeded while doing nothing.

The constraint that shaped the invite fix is sharper here: a create-time snapshot is a
poor fit for an action whose whole point is to take effect *now*. Three shapes were
weighed.

- **Refuse in isolated mode and tell the operator what to do instead.** A clear refusal
  beats a silent no-op, and it was rejected only because the advice does not exist. The
  effective manual route is to remove the member from `kenward.yaml` and restart, which
  stops their pod being created but leaves the binding in the volume — and pods are never
  purged, so re-adding that member later resumes service for the revoked account with no
  new claim. The alternative advice, deleting the pod's volume, destroys the member's lore
  to clear one line of JSON.
- **Stop or recreate the member's pod from `kenward revoke`.** `Recreate` preserves the
  volume and is reachable, but not from this process: `revoke` is a CLI invocation and the
  supervisor is a different process that would see the container die and rescue it within
  one poll. Two processes owning one container is a worse failure than the one being
  fixed.
- **Record the revocation on the host and have the pod apply it to its own state on the
  way up.** Chosen. It rides the channel the claim code already uses, one-way and
  create-time, and the pod — the process that owns that file — is the one that writes it.

So `kenward revoke` writes `<data_dir>/revocations/<id>.json`, holding the member id and
the time and nothing else. `Isolated` provisions it to `/etc/kenward/revoked.json` in that
member's pod at create time; compose mounts it read-only. `run --revoked` applies it before
the pod decides whether it has a member to serve, clearing the binding from the state file
on its own volume.

Three things it does deliberately:

- **The id in the record is checked against the pod's own member.** The compose path
  mounts this by hand, and a path pointing at the wrong file would otherwise unbind
  whoever that pod serves. Same refusal as a pod started for a member it does not serve.
- **A binding newer than the record is kept.** A member who is revoked, invited again and
  claims again holds one, and their pod is recreated on every rolling update; unbinding on
  sight would make re-enrolment impossible for exactly the people who have been through
  this once.
- **The record stays after it is applied.** It is a fact about what the operator did, not
  a queue, and the timestamp is what stops it acting twice.

**The cost, which is real and is a security cost rather than a convenience one: the
revocation lands when the pod is next created, and until then that pod is still serving
the account.** There is no channel from a CLI invocation to a running supervisor, so the
restart is the operator's, and `kenward revoke` says so in its own output rather than
leaving it to be discovered — leading with *"is NOT unbound yet"* rather than the
opposite. A deferred revocation an operator knows about is a gap they can close in one
command; a completed-looking one they cannot is the defect this replaced.

**And "restart kenward" had to be made true before it could be said.** A restart does not
create anything: `ensureRunning` starts a container that already exists, so a pod came
back on the container-layer `/etc/kenward` it was built with and never saw the record —
`>>> NOT IN THE POD <<<`, and `enrolled=true` in its own log. Found by running the mode
against real podman, and invisible to every test in the module, because a fake backend
has no container layer to be stale. So `Start` now recreates the pods that could be
holding a stale copy — a revoked member's, and an unclaimed member's with a code
outstanding — before the monitors run (`recreateStalePods`). Recreation preserves the
work volume, so no member's lore moves; it does not wait for the replacement, because the
image is unchanged and holding every other member's monitor behind one crash-looping pod
would be a worse trade than a rolling update makes. The same gap made `kenward invite`'s
own "restart kenward before handing the code over" false, and it is the same fix.

**Once, though, and not on every start.** The record is never deleted — nothing can know
when a pod has consumed it — so recreating on its mere existence rebuilt that member's pod
at every start for as long as they stayed revoked. What separates the two cases is the
pod's age: keel reports each sandbox's creation time (`sandbox.Status.CreatedAt`, which
`Start` leaves alone and `Recreate` advances), so a pod created after the record's mtime
was given that record at create time and is left alone, and one created before it cannot
have been and is replaced. Every uncertain answer replaces: a creation time the backend
cannot supply, a file that will not `stat`, a filesystem whose timestamps are too coarse
to separate the two (a two-second tolerance covers the worst of those). A needless rebuild
costs one container and preserves the work volume; a missed one leaves a revoked member
being served, and those are not comparable. Measured against real podman 4.9.3: with a
record in place the first start rebuilds the pod — new container id, later `Created` — and
the second and third leave it untouched, while the pre-change binary rebuilt it on all
three.

**And in both modes it refuses while `kenward.yaml` declares that member's
`telegram_id`.** That is the same silent success arriving by a different route: a
hand-written `telegram_id` is not in the enrolment record, so clearing the record around it
changes nothing the next start reads. kenward does not rewrite the configuration (§4), so
it names the line to delete and stops before anything has been cleared.

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

The requirements above are the destination. Most of them are now reached; one is not,
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

**Wired, but not from inside the updater: one member at a time.** The obvious
implementation does not exist and cannot. keel's cross-process lock serialises processes
that share **one install path** — several pods running off one mounted binary — and it is
taken *before* the drain, so a process that loses it skips the cycle quietly without
having silenced anybody first. But containerised isolated-mode pods each carry a private
copy of the binary in their own image filesystem. They never contend for that lock, so it
delivers no per-member sequencing for them at all. Rolling one member at a time is not
expressible from inside the process being updated. It belongs to whatever owns the pods'
shared artifact — the image, and the process that rolls it — which is the **host**
supervisor, `supervisor.Isolated`, and that is where it lives.

`Isolated.Roll` recreates each pod in turn: graceful stop, recreation from the pod's full
spec on the current image, then a wait for the new pod to hold running across two polls
before the next is touched. Members in configuration file order, the household group
last, so the pod every member shares stays on the working old image until every member's
own pod has proven the new one. **It stops at the first failure** and leaves every later
pod untouched on its working old image, because a rolling update that keeps rolling
through failures turns one broken member into a broken household. Recreation goes through
`sandbox.Recreate`, which preserves the work volume structurally; `Purge`, the one call
that would take a member's lore with it, is not reachable from this path.

**What fires it is a startup comparison, and it has to be.** The pod's image is not
observable — keel's `Inspect` reports whether a container runs, never what it was built
from — and this process does not learn it is a new build at the moment it becomes one,
because the build that swapped the binary has already exited. So the host writes down
which image it last brought the pods up on (`pod-image`, beside the state file under the
data directory) and, on the next `Start`, compares it with the image it would now start
pods from. Different, or no record at all, means roll. `ensureRunning` cannot do this job
and must not learn to: it starts a pod that exists and creates one that does not, and a
restart path that recreated pods would roll a new image across the whole household on the
first crash. A pod that does not exist yet is skipped rather than recreated — it is on no
image, and its own monitor will create it on the current one.

A failed or partial roll is logged and never fatal, and it leaves the record unchanged so
the next start tries again rather than recording a state the household is not in. The
household serves throughout, on whichever mixture of images it ended up with: the rule
that nothing in the update path may stop kenward serving outranks arriving at one
version.

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

Beyond the tier-chain refusal, the router's other failures are classified into four
notices, chosen so that each one tells the member the truth about what they can do next:

| What happened | What is sent |
| --- | --- |
| Rate limited (HTTP 429) | "The model is busy right now. Try again in a moment." |
| A rejected key, an unknown model, a request the endpoint will not parse (400, 401, 403, 404, invalid request) | "Something is wrong with this household's setup — tell whoever runs it." |
| The model spent the turn thinking and never answered (empty response carrying a reasoning trace) | "The model spent the whole turn thinking and didn't get to an answer. Nothing is broken — try asking again, or in smaller pieces." |
| Anything else | "Something went wrong reaching the model, and your message wasn't answered. Try again in a moment." |

The setup row is the one that earns its place: no amount of retrying fixes a rejected
key, the member cannot repair it, and the operator can — so the notice sends them to the
person who can. **400 belongs in that row, not in the transient one.** It is the endpoint
saying it will not parse this request — the same permanent fault as `ErrInvalidRequest`,
caught one hop later — and telling a member to try again for it sends them back to a
wall.

The thinking row exists because the alternative was a lie. Routing declines to fail over
on a reasoning-only turn (§7), so it reaches this classifier instead of exhausting the
chain — and before that split existed, a member whose local model thought its way past
the token budget was told *no machine in your allowed tiers is reachable*, naming healthy
machines as unavailable while the one that answered sat in cooldown for the next turn.
The notice says the two things that are true: nothing is broken, and a smaller question
is the one lever the member actually holds. More room to answer in is the operator's
knob, not theirs, so it is not offered as advice they cannot take.

Five further notices come from outside the router, sent by the Unit around a turn rather
than as part of one. They are product surface exactly as the refusals are.

| When | What is sent |
| --- | --- |
| The model declined the turn (content filter) | "The model declined to answer this." |
| A direct message arrives while the member's key is locked | "Your assistant is locked. It needs to be unlocked on the machine it runs on." |
| A message has waited behind a running turn for longer than `QueueNoticeAfter` | "Still working on your last message — this one is queued and I'll take it next." |
| A message arrives when the queue behind a running turn is already full | "I'm backed up and had to drop that message. Send it again in a moment." |
| The turn ran to the end and produced nothing the member could see | "I didn't get a usable answer to that. Try asking again." |

The two queue notices exist because turns are serialised (§5) and a member cannot see
that. Waiting in silence and being ignored look identical from inside a chat; so do a
dropped message and a lost one. Both notices are sent only after the scope has resolved,
so neither ever reaches a stranger.

The last row covers the two ways a turn can succeed and still say nothing: a completion
with no text and no tool call, and a bare tool call whose capture proposal was suppressed
without asking. Neither is a failure the node can classify further, and neither may be
answered with silence — this section promises every message produces something, and until
this notice existed that promise had two paths where it was untrue.

The generic notice is the last resort, not the mechanism. A turn that ends in a capture
question — a remember proposal or a publish request — hands the rest of the turn to
`internal/capture`, and **every error path there that a member could be waiting on sends
its own notice first**, because the engine knows what failed and the generic notice does
not. Those errors are marked with `capture.ErrMemberNotified`; the assistant falls back
to "I didn't get a usable answer to that" only when the marker is absent and the turn
produced no reply of its own. The marker is what keeps the member from being told two
different things about one failure.

This matters most in the publish flow, where the model is instructed to answer with a
bare tool call and the member's tap authorises something irreversible. A failure to
resolve the shared space, to read the entry back, or to put the question is reported as
nothing having been published. A failure of `Share` **after** the member has tapped
*Publish to household* is reported as uncertainty — "I can't confirm whether … was
published … a publication can't be taken back" — on the same reasoning as an uncertain
write in section 12: the copy may have landed, its id is in the answer that never came,
and a member who is told a flat failure will simply publish again.

It matters as much now in the private-write flow, where the member is not waiting on a
question at all. Three failures there are members' business rather than the operator's,
and each has its own sentence in §6: the write may not have landed, the announcement may
not have gone out, and the undo may not have removed anything. The one that cannot be
allowed to fall back to a generic notice is the middle one. An entry is in the store and
the message saying so did not reach the chat; "I didn't get a usable answer to that"
would be a silent write with a polite apology attached, so the engine sends a plain
confirmation instead and says the undo button is missing rather than implying one
exists.

The router notices, the content-filter decline, the locked notice and the "no usable
answer" notice are golden-tested alongside the refusals, under
`internal/assistant/testdata/`. The two queue notices are asserted in
`assistant_test.go` against the serialisation behaviour that produces them rather than
held in a golden file, because what needs pinning there is *when* each is sent — after
the notice delay, and at the limit — and a golden of the text alone would not pin it.

The classification reads `keel/llm`'s error vocabulary, which the routing seam passes
through unchanged; a content-filter decline commonly arrives as an empty response
carrying the finish reason rather than as a completion with text, and a reasoning-only
turn always arrives that way. A decline is checked first: a model that reasoned its way
to refusing still refused, and the refusal is the more important thing to say.

---

## 11. Testing

`TESTING.md` is the full account, including what has been run by hand and what remains
unproved. This section is only the part that binds: the tests a change may not remove.

- `scope` has an exhaustive table test: `(config, inbound) -> Scope | rejection`. This
  is the security test of the whole product; it must cover unknown users, unknown
  chats, a member messaging from a different chat, and a group id that collides with a
  member's private space.
- `routing` is tested against a fake endpoint set: cooldown behaviour, probe caching,
  tier fallthrough, the three-way split of an empty response (§3) with the healthy
  sibling counting the requests that reach it, and the assertion that an exhausted chain
  **never** reaches an endpoint outside it.
- `capture` is tested as a state machine including timeout-as-decline, the
  taps-from-the-wrong-user case, and every ending of the undo window: the delete that
  worked, the one lore refused, the one lore never answered, and the one nobody tapped.
  The memory double carries `deleteErr` for that, because the two endings that are not
  "gone" do not exist unless the double can produce them.
- `memory`'s parser is tested against a scripted fake MCP server over a corpus of 31
  captured fixtures in `internal/memory/testdata`. A format change must fail there, in
  one place, rather than in whatever calls it.
- Refusal strings, rendered prompts and the privacy statements are golden files, so
  softening one is a visible edit to a fixture rather than an accident.
- `internal/e2e` runs whole messages through the production wiring. Its
  `telegram_test.go` drives the real `go-telegram/bot` client over real HTTP against
  `internal/transport/telegramtest`, a loopback Bot API server implementing Telegram's
  real `getUpdates` offset semantics, real inline keyboards and real `callback_query`.
  **None of `internal/e2e` except `live_test.go` is tagged**, and that is deliberate: it
  needs nothing but the test binary, so it runs on every commit, which is the only reason
  it caught what it caught.

Tests needing equipment — real Podman, a real lore, a real model — are tagged
`//go:build integration` and excluded from the default `go test ./...`. There are four:
`internal/e2e/live_test.go` (real `lore mcp` **and** a real endpoint, faking only
Telegram), `internal/memory/integration_test.go`, `internal/setup/spaces_lore_test.go`,
and `internal/supervisor/isolated_integration_test.go` (also `&& linux`). Each skips when
its equipment is absent, so a green integration run on a bare machine proves nothing.

Any integration test that touches lore must create its own `LORE_HOME` under `t.TempDir`
and its own spaces inside it. A test pointed at a persistent store accumulates its own
writes until they crowd out the entry the run just made — a test that corrupts what it
measures gets less trustworthy every time it runs. `lore_delete` does not change that
rule: it is not tidy-up machinery for a suite, and a test that cleaned up after itself by
deleting would still be one crash away from leaving the store dirtier than it found it.

---

## 12. What lore actually does

Established by reading lore's source, not by assumption. These facts constrain the
design and several of them contradict what the architecture originally supposed.

- **`lore mcp` is stdio only**, exposes six tools (`lore_search`, `lore_get`,
  `lore_put`, `lore_spaces`, `lore_share`, `lore_delete`), and returns **unstructured
  text**, not JSON.
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
  run on one host, each holding a subset of spaces. This is what makes one lore per
  member pod viable in isolated mode.
- **Separate `LORE_HOME`s do not converge on a shared space by themselves**, and this
  document used to say they did. One `LORE_HOME` is one lore *account*, and `lore init`
  gives each account its own space ids, so the id `household.shared_space` names exists
  in exactly one store. In every other pod `doctor` reports `space "…" is not a space
  this lore store holds` and that conversation reads nothing, silently, because a turn
  that cannot read a space degrades that space rather than failing. Convergence is
  lore's own sharing — `lore space invite`, `lore join`, and a reachable `lore serve` —
  and **nothing in kenward calls any of the three**: not `internal/supervisor`, not
  `internal/setup`, and `deploy/compose.isolated.yml` defines no port, peer or daemon.
  Private spaces are unaffected. Treated as an open limitation rather than fixed,
  because D-036 changes what the fix looks like.
- **Sync is last-writer-wins per entry**, compared on `(updated_at, author_account)`.
  It is not a CRDT: the losing version is discarded silently, with no conflict record.
  A machine with a fast clock wins every conflict. Household clocks should be synced,
  and nothing in kenward may assume a write it made is still there.
- **`lore mcp` alone never syncs.** Syncing requires a separate `lore serve`. Any
  deployment running more than one lore instance must run both.
- **Invites are not exposed over MCP.** Enrolment drives the lore CLI, which has
  non-interactive flags but emits no JSON.
- **Delete exists, by tombstone, and only by id.** `lore_delete(id, space)` writes a
  signed tombstone that propagates to every synced device; the entry then stops coming
  back from `lore_search` and `lore_get`. It is space-scoped — unlike `lore_get`, and by
  comparing space *ids* rather than the display names kenward's own check falls back on,
  so it is the stronger of the two guards. Deleting an already-deleted entry is a no-op
  lore reports rather than an error.

  What it does not give kenward is a way to clean up after a write whose answer was
  lost. The id comes back in the receipt, so a write with no receipt leaves nothing to
  name, and the uncertain-write rule below is unchanged by it.
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
  though the client reported failure, and the id that would let kenward delete it was in
  the answer that never arrived, so a retry that duplicates it is permanent. Report
  uncertainty to the member rather than silently retrying.

  `Delete` is issued as a write for the same reason, even though repeating a tombstone
  is harmless: the flag also decides whether a lost answer comes back wrapped in
  `ErrWriteUncertain`, and after an undo the difference between *"it is still there"* and
  *"I cannot tell"* is the entire message the member gets. Buying one free retry by
  giving that distinction up is the wrong trade.
- `confidence` ∈ {experimental, provisional, validated, hardened} and `origin` ∈
  {evidence, directive, convention, constraint}, both enforced by lore. **Markers are
  free-form strings** — the familiar vocabulary is convention only, so kenward must not
  validate against it.
- lore's SQLite runs WAL with a single connection and a 5s busy timeout, so concurrent
  calls contend. Client concurrency is bounded and busy errors are retried.

## 13. Non-goals

Not built, and not designed around: billing, tenant orchestration, a control plane, SSO,
organisations and teams, usage quotas, and any multi-tenant runtime. One container per
household is what makes all of these bolt-on rather than structural.

**A web dashboard is no longer on that list.** D-035 reverses it: the operator this
project is aimed at is now a stranger who downloaded an app rather than the author of the
configuration, and everything kenward asks of an operator has no interface for that
person. The reasoning, and the exposure rules that come with opening the design's first
inbound port, are in `ARCHITECTURE.md` under **Non-goals**. It is decided and unbuilt;
nothing in this contract binds it yet, and the parity rule it brings — every operator
action gets a CLI implementation too, because headless stays first-class — is the part
that will.

*"Automatic memory writing without confirmation"* has also left this list, and its removal
is not a softening: D-038 replaces the confirmation on a **private** write with an
announcement carrying an Undo, and leaves the approval on a **shared** write untouched and
non-configurable. §6 is the binding text; it is being rewritten alongside the code.
