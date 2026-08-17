# Releasing

kenward updates itself from a signed manifest. That makes the release signing key the
root of trust for every installation: anyone holding it can execute code on every
household that updates. Nothing in the updater compensates for a leaked key, so the
handling below is not ceremony.

---

Release tooling is a **separate binary**, `cmd/kenward-release`, and is deliberately not
shipped in the container image or the release artifacts. A household's copy of kenward
has no reason to be able to generate signing keys or sign manifests, and every capability
present in a widely-installed binary is one an attacker gets to use.

It has four subcommands: `keygen` makes a keypair, `manifest` builds an unsigned manifest
from a directory of artifacts, `sign` adds a signature to one, and `verify` reads a
signed manifest back with the same code the updater runs.

---

## The signing key

An Ed25519 keypair. The **public** half is compiled into the binary; the **private** half
signs release manifests and must never be on a build machine, in CI, or in this
repository.

Generate once:

```sh
kenward-release keygen --out ~/.kenward-release/
```

This writes `release-<id>.key` (0600) and `release-<id>.pub`. The id goes into the
manifest so a verifier knows which key to check against.

**Where the private key lives:** offline, on removable media or a hardware token, backed
up somewhere that is not the same failure domain as the machine you sign on. If it is
lost, existing installations keep working but can never be updated again by you — they
will refuse manifests signed by anything they do not already trust. If it is *stolen*,
the situation is worse and the remedy is unpleasant, so see rotation below.

**CI never signs.** The build is reproducible and public; the signature is applied by a
human on a machine holding the key. A CI system that can sign is a CI system that can
push code to every household, which is precisely the property the signature exists to
deny.

---

## Trusting more than one key

keel's updater accepts a manifest signed by **any** of the public keys compiled into the
binary, which is what makes rotation possible without stranding anyone:

1. Ship a release trusting both the old key and the new one, signed with the **old** key.
2. Wait until that release has propagated — long enough that installations on the stable
   channel have taken it.
3. Start signing with the new key.
4. In a later release, drop the old key from the trusted set.

Skipping step 2 strands every installation that had not yet updated, because it will not
trust a manifest signed by a key it has never heard of. There is no way to recover those
except by hand.

---

## Cutting a release

A release happens in two halves, and the split is the whole point: **CI builds and
publishes nothing that a household will act on; a human signs.**

### 1. Tag it

Set the version once and use it throughout; the commands below assume it. `v0.1.0` is the
only tag that exists so far.

```sh
VERSION=v0.1.1

git tag -a "$VERSION" -m "$VERSION"
git push origin "$VERSION"
```

That is the only command that starts a release. `.github/workflows/release.yml` then:

- reruns the full gate — gofmt, vet, `go build ./...`, `go test -race` — on the exact commit
  the tag names, because a tag can point at anything, including a commit that never saw a
  pull request. The gate runs on **Linux, macOS and Windows**, the same three platforms as
  `ci.yml`, and the tag validation and version stamp live in a separate one-runner `stamp`
  job ahead of it (a matrix cannot own the stamp: matrix job outputs are last-writer-wins,
  so the version would depend on which runner finished last);
- builds six binaries (linux, darwin, windows × amd64, arm64) with `CGO_ENABLED=0`,
  `GOWORK=off` and `-trimpath`, stamping `internal/version` from the tag;
- publishes them, plus `.tar.gz`/`.zip` archives, `.deb` and `.rpm` packages,
  `kenward.service`, `checksums.txt` and `install.sh`, to a **draft** GitHub release.
  `kenward.service` has a line in `checksums.txt` and is published for one reason:
  `install.sh` writes it into `/etc/systemd/system` as root, so it must be verifiable
  the same way the binary is. It used to be fetched from the tip of `main` and checked
  against nothing;
- builds the desktop wrapper on three runners — `.deb` and `.rpm` on Linux, a universal
  `.dmg` on macOS, an Inno Setup installer on Windows — attaches them to the same draft
  and extends `checksums.txt` to cover them. This is outside GoReleaser because
  `cmd/kenward-desktop` needs cgo on darwin and cgo does not cross-compile;
  `docs/DESKTOP.md` has the whole reasoning. The daemon inside each bundle is a
  separate build of the same commit, stamped identically but not byte-identical to the
  published `kenward_<goos>_<goarch>` — the bundle is checksummed, the copy inside it
  is not separately;
- pushes `ghcr.io/blueheisenberg/kenward:$VERSION` (and `:latest`, unless the tag carries a
  hyphen and is therefore a prerelease) for linux/amd64 and linux/arm64.

The release is a draft because `kenward update` fetches
`.../releases/latest/download/manifest.json`, and "latest" means the newest *published*
release. Publishing binaries before the signed manifest is attached would hand every
household a release whose manifest 404s.

The workflow can also be run by hand from the Actions tab on a branch: it then builds a
snapshot, publishes nothing, and uploads the artifacts for inspection. Locally, the same
rehearsal is `task snapshot`.

#### What the three-platform gate costs

Three cold builds instead of one, and — the part worth knowing before you are blocked by
it — **every release now depends on GitHub's macOS and Windows runners being available**.
A runner outage on either platform stops a release that would previously have gone out.

That is the right trade here and it would not be everywhere: the thing at the end of this
pipeline is a *draft* waiting on a human signature, so a release delayed by an hour costs
nothing, while a release cut from a tree that is red on macOS costs a household. The gate
was single-platform until two real races — one in a Telegram test fake, two `wg.Add` calls
outside the lock in the supervisors — failed macOS and Windows for four consecutive CI runs
while Linux stayed green. For those four runs the tree was taggable, and a tag would have
produced a green release from it.

If a runner outage is genuinely blocking a release you must cut now, the honest move is to
wait, not to make the gate lenient again.

### 2. Sign it, attach the manifest, publish

On the machine holding the private key, which is not a build machine and not CI:

```sh
VERSION=v0.1.1        # the tag you just pushed; this is a different machine

mkdir -p /tmp/rel && cd /tmp/rel
gh release download "$VERSION" --repo BlueHeisenberg/kenward --pattern 'kenward_*'

kenward-release manifest --version "$VERSION" \
    --channel edge,stable \
    --dist . \
    --notes "..." \
    --out manifest-unsigned.json
kenward-release sign --key ~/.kenward-release/release-1.key \
    --in manifest-unsigned.json --out manifest.json
kenward-release verify --in manifest.json --pub ~/.kenward-release/release-1.pub

gh release upload "$VERSION" manifest.json --repo BlueHeisenberg/kenward
gh release edit "$VERSION" --draft=false --repo BlueHeisenberg/kenward
```

Download the artifacts rather than hashing a local `dist/`: the digests then cover exactly
the bytes GitHub will serve, not bytes that ought to be the same. The archives and packages
that come down with them are ignored — `manifest` only recognises files named
`kenward_<goos>_<goarch>`.

**Keep the `kenward_*` pattern.** `manifest` reads a platform out of any filename shaped
`*_<goos>_<goarch>`, on purpose, so renaming the binary does not silently drop every
artifact from the manifest. The desktop bundles are named `kenward-desktop_*` and
`kenward_<tag>_macos.dmg`, none of which match `kenward_*` followed by a bare
`<goos>_<goarch>` — but widen the pattern to `kenward*` and a `kenward-desktop_linux_amd64`
would parse as `linux/amd64` and collide with the daemon. It fails loudly rather than
producing a wrong manifest; it still costs you the download. The desktop wrapper is not
in the manifest and is not meant to be — `docs/DESKTOP.md`, "The wrapper is not in the
update manifest", says why.

The signed envelope must be attached under the name `manifest.json`, because that is the
filename `releaseManifestURL` in `cmd/kenward/release.go` asks for.

`--channel` takes a list, because the Channels section below says to publish once to both:
`--channel edge,stable` writes the same release into each and lets the client-side delay
separate them. Artifact URLs default to
`https://github.com/BlueHeisenberg/kenward/releases/download/{version}/{file}`, which is
where the workflow puts them; override with `--base-url` if artifacts are hosted elsewhere.
`--published-at` overrides the timestamp, which matters only for reproducing a manifest
exactly.

### The public key has to be in the build

`cmd/kenward/release.go` ships with an empty trusted set, on purpose. A build that trusts
nothing refuses to update rather than fetching something it cannot verify — correct, and
useless to a household.

Set the repository **variable** (not secret — it is compiled into every published binary
anyway) `KENWARD_RELEASE_KEYS` to the base64 public key `keygen` printed, comma-separated
if more than one is trusted during a rotation:

```sh
gh variable set KENWARD_RELEASE_KEYS --repo BlueHeisenberg/kenward --body 'BASE64KEY'
```

The workflow passes it to `-X main.releaseTrustedKeys`. Nothing else in CI ever touches key
material, and the private half never appears there in any form.

### Homebrew

`.goreleaser.yaml` generates a cask on every run and does not push it. As of v0.1.0 it
never has, and `brew install --cask BlueHeisenberg/tap/kenward` fails for everyone who
tries it.

`github.com/BlueHeisenberg/homebrew-tap` **exists** — public, `main`, containing
`Casks/.gitkeep` and nothing else. The repository is not the missing piece and creating
another one will not help. What is missing is a credential and a switch, both on
`BlueHeisenberg/kenward`, and neither can be created by CI or from this repository:

1. **A token that can write to the tap.** A GitHub *fine-grained* personal access token:

   - Resource owner: `BlueHeisenberg`
   - Repository access: **Only select repositories** → `BlueHeisenberg/homebrew-tap`
   - Repository permissions: **Contents: Read and write**. Nothing else. (Metadata:
     Read-only is added automatically and cannot be removed.)
   - Expiry: your choice, but note that when it expires the release stops publishing a
     cask and says nothing, because `skip_upload` failing open is the whole design.

   A classic token works too and needs `public_repo` (the tap is public), but it is a
   credential for every repository the account can reach and there is no reason to
   accept that here.

   The token must **not** be able to write to `BlueHeisenberg/kenward`. It is handed to
   GoReleaser in the same process that holds `GITHUB_TOKEN`; keeping the two disjoint
   means the tap credential cannot touch a release.

2. **Store it as a secret and flip the switch:**

   ```sh
   gh secret set HOMEBREW_TAP_TOKEN --repo BlueHeisenberg/kenward       # paste the token
   gh variable set HOMEBREW_TAP_SKIP --repo BlueHeisenberg/kenward --body false
   ```

   `HOMEBREW_TAP_SKIP` is a **variable**, not a secret — it is the string `false`, and
   the workflow reads `${{ vars.HOMEBREW_TAP_SKIP || 'true' }}`, so unset means skip.
   `HOMEBREW_TAP_TOKEN` is a **secret**. Setting one without the other does nothing
   useful: with the variable set and no token, GoReleaser tries to push with an empty
   credential and the release job fails partway through.

Nothing else is required. The tap needs no workflow, no formula, no `README`; GoReleaser
commits `Casks/kenward.rb` to `main` itself.

**Until then, by hand.** The cask is written to `dist/homebrew/Casks/kenward.rb` on every
run including `task snapshot`, with the real URLs and digests. Copying that one file into
the tap and pushing it produces exactly what the automated path would, and needs no
token at all — it just has to be redone every release, which is why it is the fallback
and not the plan.

**Timing.** GoReleaser pushes the cask while the GitHub release is still a draft, and the
URLs inside the cask 404 until it is published. Sign and undraft promptly; the window is
however long signing takes you.

The manifest carries, per channel: the version, release notes, publication timestamp, a
`securitySensitive` flag, and for each platform an artifact URL, SHA-256 and size. The
signature covers the whole payload, so the digests cannot be swapped independently of it.

### Reading back what you are about to publish

```sh
kenward-release verify --in signed.json --pub ~/.kenward-release/release-1.pub
```

`verify` checks the envelope with the same code the updater runs and then prints
everything it says: which of the keys you gave it signed the manifest, and per channel
the version, the publication time, the `securitySensitive` flag, the notes, and every
artifact with its digest.

Run it on the file you are about to publish, and read the output rather than glancing at
the exit code. Everything below the signature line is what every household will act on,
and a manifest is the one artifact where a typo reaches other people's machines
automatically.

`--pub` repeats. A key that did not sign is reported rather than treated as a failure,
because during a rotation you are expected to pass both the old and the new one; the
exit is non-zero only when **no** key signed the manifest.

### `securitySensitive`

Set this on any release that changes routing behaviour, tier defaults, key handling, the
enrolment path, or what any privacy statement claims. A release so flagged **will not
auto-apply** — it asks the household first, regardless of the version bump.

The rule exists because the worst bug this project could ship is a release that quietly
makes a local-only space reach a cloud provider. If you are unsure whether a change
qualifies, it qualifies.

---

## Channels

| Channel | Who runs it | Behaviour |
| --- | --- | --- |
| `edge` | The maintainer's own household | Takes releases immediately |
| `stable` | Everyone else | Takes the same release after `StableDelay` |

The delay is enforced client-side against the manifest's publication timestamp, so a
release cannot be pushed to stable early by re-publishing it. Publish once, to both
channels, and let the delay do its work.

The point of the arrangement is that the maintainer's own family finds the breakage. If
you are not running `edge` at home, the stable channel is not being tested by anyone.

---

## Before you publish

- [ ] CI green on all three platforms
- [ ] The draft release carries all six `kenward_<goos>_<goarch>` binaries, and each one
      runs `kenward version` on its own platform — the updater's preflight will execute
      exactly this before swapping, and a binary that fails it refuses the update rather
      than bricking the install
- [ ] The draft release carries the three desktop bundles — the `.dmg`, the
      `_setup.exe`, and four packages — and `checksums.txt` has a line for each. A
      failed `desktop-publish` job leaves the release looking complete and quietly
      missing them
- [ ] `kenward-desktop` launched once from at least one bundle, and its icon appeared.
      A tray icon is the one thing no test can assert
- [ ] `KENWARD_RELEASE_KEYS` is set, so the published binaries can verify anything at all
- [ ] Major version implies the update will ask for consent
- [ ] `securitySensitive` set if anything in the list above changed
- [ ] Release notes say what changes for a household, not what changed in the code
- [ ] Manifest signed on a machine holding the key, not in CI
- [ ] `kenward-release verify --in manifest.json --pub <every trusted key>` run on the
      exact file about to be published, and its output read — the signature, the
      versions, the flags and every digest
- [ ] `manifest.json` attached to the draft release **before** it is undrafted
- [ ] Installed the release on your own household from `edge` and used it for a day

---

## If the key is compromised

There is no revocation mechanism, deliberately: a revocation channel is another thing an
attacker who controls the update host can manipulate. What you have instead:

1. Take the manifest URL offline immediately. Installations that cannot fetch a manifest
   do not update; they keep running the version they have, which is the correct failure
   mode.
2. Publish, out of band, that the key is compromised and that installations must be
   updated by hand.
3. Ship a new build trusting only a new key. Every household has to install it manually,
   because by definition you cannot reach them through the channel the attacker owns.

Say this plainly in the security advisory rather than minimising it. A privacy-focused
product that soft-pedals a signing key compromise deserves the reception it gets.
