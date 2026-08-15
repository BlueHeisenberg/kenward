// Command kenward-release is the release tooling for kenward: signing key
// generation, manifest construction, signing, and verification.
//
// It is a separate binary, and it is deliberately never shipped in the
// container image or in the release artifacts. A household's copy of kenward
// has no reason to be able to generate signing keys or sign manifests, and
// every capability present in a widely-installed binary is one an attacker
// inherits. Keeping the signing capability out of the shipped binary costs
// nothing and removes it from every machine that does not need it.
//
// For the same reason this binary depends on keel/update (the manifest types
// and signature primitives it must agree with byte for byte) and on kenward's
// own internal/version, and on nothing else from kenward.
//
// The release signing key is the root of trust for every installation:
// whoever holds it can execute code on every household that updates. Nothing
// in the updater compensates for a leaked key. Accordingly this program
// never prints, logs, or copies a private key: it is written once, at mode
// 0600, to the path the operator named, and read only to sign. An existing
// key file is never overwritten under any flag, because a key silently
// replaced is a key lost, and losing it means no installation can ever be
// updated again.
//
// See docs/RELEASING.md for the procedure this tooling exists to serve.
package main
