# Releasing

kenward updates itself from a signed manifest. That makes the release signing key the
root of trust for every installation: anyone holding it can execute code on every
household that updates. Nothing in the updater compensates for a leaked key, so the
handling below is not ceremony.

---

## The signing key

An Ed25519 keypair. The **public** half is compiled into the binary; the **private** half
signs release manifests and must never be on a build machine, in CI, or in this
repository.

Generate once:

```sh
kenward release keygen --out ~/.kenward-release/
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

```sh
task cross                                   # build every platform into dist/
kenward release manifest --version v0.2.0 \
    --channel edge \
    --dist dist/ \
    --notes "..." \
    --out manifest.json
kenward release sign --key ~/.kenward-release/release-1.key --in manifest.json --out signed.json
```

Then publish `signed.json` at the manifest URL and the artifacts at the URLs it names.

The manifest carries, per channel: the version, release notes, publication timestamp, a
`securitySensitive` flag, and for each platform an artifact URL, SHA-256 and size. The
signature covers the whole payload, so the digests cannot be swapped independently of it.

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
- [ ] `task cross` produced every artifact, and each one runs `kenward version` on its
      own platform — the updater's preflight will execute exactly this before swapping,
      and a binary that fails it refuses the update rather than bricking the install
- [ ] Version bumped; major version implies the update will ask for consent
- [ ] `securitySensitive` set if anything in the list above changed
- [ ] Release notes say what changes for a household, not what changed in the code
- [ ] Manifest signed on a machine holding the key, not in CI
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
