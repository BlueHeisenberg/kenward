# kenward — project instructions

Read `README.md`, `docs/ARCHITECTURE.md`, `docs/IMPLEMENTATION.md` and `docs/PROMPT.md`
before changing anything. `docs/IMPLEMENTATION.md` is the build contract: where an
implementation disagrees with it, the document gets fixed deliberately rather than
drifted from.

Family-level reasoning and the decision log live in the sibling knowledge repository
(`hearth-design`, pending rename), in `docs/DECISIONS.md`. Do not restate decisions
here; link to them.

## Ground rules

- **Git identity**: commit as `BlueHeisenberg <2033896+BlueHeisenberg@users.noreply.github.com>`.
  Never as any other personal account or the work identity — the global git config on this
  machine is the work one, so set it locally.
- **Remotes** use the SSH host alias `github-personal`
  (`git@github-personal:BlueHeisenberg/<repo>.git`). Plain `github.com` SSH resolves to
  the wrong key.
- **Sole copyright.** No outside contribution is merged before a CLA exists. This is not
  ceremony: lifting code between the family's repositories depends on it.
- **Household configuration and secrets never enter version control.** `.gitignore`
  covers `kenward.yaml`, tokens, keys and state directories. Publication stays a
  decision, not a history rewrite.

## Architectural rules that are not negotiable

These encode properties that cannot be retrofitted. Breaking one is a design change, not
a refactor.

- **The per-member assistant is an isolated unit.** No shared mutable state keyed by
  member id, anywhere. This is what lets the same code run as goroutines in Simple mode
  and as pods in Isolated mode.
- **`domain.Scope` is the authorization boundary.** Resolving a message to a Scope is
  the decision; everything downstream obeys it and re-derives nothing. A group Scope may
  never name a private space.
- **Routing never widens a tier chain.** No default, no fallback, no "if nothing matched,
  try everything". The chain is the privacy policy, and the test proving a cloud
  endpoint receives zero requests under a local-only chain must keep passing.
- **Nothing is written to memory without an explicit member confirmation**, and there is
  no configuration option to disable it.
- **A group conversation never offers a personal capture destination.**
- **Simple mode is described honestly.** Separation between members is real in both
  modes; sealing against the operator exists only in Isolated. Never use sealed-memory
  language for Simple mode.

## Dependencies

Third-party modules are fixed in `docs/IMPLEMENTATION.md`. Adding one is a decision
recorded in `docs/ARCHITECTURE.md`, not a `go get`.

`github.com/BlueHeisenberg/keel` is the shared core. If something belongs there rather
than here, the test is whether its signature needs a domain noun — if it does, it stays
in kenward.

## Working style

- Tests come with the code, table-driven, no network in unit tests. Integration tests
  needing real Podman or a real lore are tagged `//go:build integration` and excluded
  from the default run.
- `gofmt` and `go vet` clean before committing.
- Refusal strings and rendered prompts are golden-tested. Changing one is a deliberate
  fixture edit.
