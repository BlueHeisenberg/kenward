package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// inviteStoreFileName is where minted claim-code digests live, under the data
// directory beside the enrolment state they will eventually produce.
//
// The file holds digests and expiry times and nothing redeemable, but knowing which
// invites are outstanding is knowing where to aim, which is why enrol.FileStore
// writes it 0600.
const inviteStoreFileName = "invites.json"

// inviteSeedDirName holds one file per member: the invites that member's pod is to be
// given, and no other member's.
//
// It exists because in isolated mode the store `kenward invite` mints into and the
// store a claim is redeemed against are two different files on two different
// filesystems — the host's data directory and the pod's own volume — and nothing
// crosses between them by itself. A member's file is what crosses: provisioned into
// their pod at create time by the host supervisor, or bind-mounted by the compose
// deployment, and imported there into the pod's own store.
//
// One file per member rather than the whole store, because the store holds every
// member's digests and a pod may hold nothing of anybody else's. They are digests, so
// the exposure would be theoretical; the rule is not.
const inviteSeedDirName = "invites"

// revocationDirName holds one file per revoked member: when that member's binding was
// revoked, for that member's pod and no other.
//
// It is the other direction of the same crossing inviteSeedDirName exists for, and it
// exists for the harder half. In isolated mode a claim is redeemed inside the member's
// pod and the binding is written to that pod's own volume, so `kenward revoke` on the
// host has nothing to clear: it unbound a host record the pod has never read, reported
// success, and the pod carried on serving the revoked account. The host cannot go and
// fix that itself — writing into a running member's volume is the one thing this mode
// forbids, because every mechanism that could is one edit from reading it back and that
// volume holds the member's wrapped key and their lore.
//
// So the fact travels the only way anything travels here: one-way, host to pod, read at
// create time. The pod applies it to its own state file on the way up, which is why a
// revocation in this mode takes effect when the pod is next created and not before —
// stated in the command's own output rather than left to be discovered.
const revocationDirName = "revocations"

func inviteStore(cfg *config.Config) *enrol.FileStore {
	return enrol.NewFileStore(filepath.Join(cfg.DataDir, inviteStoreFileName))
}

// revocation is what the host records for a pod to apply.
//
// It carries the member id as well as the time because the file is provisioned into
// exactly one pod, and a compose file with the wrong path in it would otherwise unbind
// whoever that pod serves. The pod checks the name before acting on it, the same way it
// refuses to be started for a member it does not serve.
//
// The time is what stops it being a standing order. A member who is revoked, invited
// again and claims again has a binding newer than the revocation, and their pod is
// recreated on every rolling update; without the comparison the second claim would be
// undone by the first revocation, forever.
type revocation struct {
	MemberID  domain.MemberID `json:"member_id"`
	RevokedAt time.Time       `json:"revoked_at"`
}

// revocationDir is the directory of per-member revocation records under the data
// directory.
func revocationDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, revocationDirName)
}

// revocationPath names one member's record under dir. It must agree with what
// internal/supervisor looks up (perMemberPath) and what the compose deployment mounts;
// TestTheTwoDeploymentPathsAgreeOnWhereARevocationIsRead is what holds them together.
func revocationPath(dir string, id domain.MemberID) string {
	return filepath.Join(dir, string(id)+".json")
}

// writeRevocation records a revocation where the member's pod will be given it.
//
// 0644 in a 0700 directory, for the reason enrol.FileStore.Readable exists: the compose
// deployment bind-mounts this exact file into a container that runs as the image's fixed
// non-root account, and a 0600 file carries its owner across the mount. There is nothing
// in it to withhold — a member id and a timestamp — and the directory keeps every other
// account on the host out.
func writeRevocation(dir string, id domain.MemberID, at time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(revocation{MemberID: id, RevokedAt: at}, "", "  ")
	if err != nil {
		return "", err
	}
	path := revocationPath(dir, id)
	return path, os.WriteFile(path, append(data, '\n'), 0o644)
}

// readRevocation reads a record. A path that names no file is no revocation and not a
// failure: most pods have never had one, and a household that has revoked nobody has no
// directory at all. A file that exists and cannot be read or parsed is a failure — a pod
// that shrugged at it would go on serving an account somebody revoked.
func readRevocation(path string) (revocation, bool, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return revocation{}, false, nil
	case err != nil:
		return revocation{}, false, err
	}
	var rec revocation
	if err := json.Unmarshal(data, &rec); err != nil {
		return revocation{}, false, fmt.Errorf("parsing %s: %w", path, err)
	}
	if rec.MemberID == "" || rec.RevokedAt.IsZero() {
		return revocation{}, false, fmt.Errorf("%s names no member or no time", path)
	}
	return rec, true, nil
}

// inviteSeedDir is the directory of per-member seed files under the data directory.
func inviteSeedDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, inviteSeedDirName)
}

// inviteSeedStore is the seed file for one member. It is an ordinary invite store, so
// the same reader and writer serve both ends of the journey.
//
// Readable, and that is the compose deployment's requirement rather than a relaxation
// of anything. That deployment bind-mounts this exact file into the member's container,
// where the process runs as the image's fixed non-root account and not as whoever ran
// `kenward invite`; a 0600 file carries its owner across the mount and cannot be opened
// on the far side. Found on real podman, where jordan's container refused to start:
//
//	kenward: importing outstanding invites from /etc/kenward/invites.json:
//	  enrol: read /etc/kenward/invites.json: open /etc/kenward/invites.json: permission denied
//
// The directory it sits in is 0700, so no account on the host gains anything; see
// enrol.FileStore.Readable for the full reasoning and for why chowning is not the
// answer. The supervisor path is unaffected either way — it reads these bytes on the
// host and provisions its own 0600 copy owned by the pod's own uid.
func inviteSeedStore(dir string, id domain.MemberID) *enrol.FileStore {
	return enrol.NewFileStore(filepath.Join(dir, string(id)+".json")).Readable()
}

// copyInvites copies the records src holds and keep accepts into dst, and reports how
// many were new. It is both halves of the journey: the host exporting a member's
// invites to their seed file, and the pod importing that seed into its own store.
//
// A record dst already holds is skipped rather than overwritten, and that is the whole
// reason this is a copy rather than a file move. The pod's store is the one that marks
// a code consumed; the seed never learns of it, because the seed is written on the host
// and consumption happens in the pod. Overwriting would restore a spent code to
// redeemable on the next pod recreation, which is a single-use code used twice.
func copyInvites(ctx context.Context, dst, src *enrol.FileStore, keep func(enrol.Code) bool) (int, error) {
	codes, err := src.All(ctx)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, c := range codes {
		if keep != nil && !keep(c) {
			continue
		}
		switch err := dst.Save(ctx, c); {
		case errors.Is(err, enrol.ErrDuplicateCode):
		case err != nil:
			return added, err
		default:
			added++
		}
	}
	return added, nil
}

// newBinder builds the Binder that a claim's binding goes through.
//
// The zero Provisioning is deliberate and is passed everywhere in this binary: it
// refuses to create a member the configuration does not declare. config.Binder can
// create one, but it cannot persist one — the state file holds bindings and nothing
// else, by design, because a member's name, space and tier chain are the operator's
// file rather than kenward's to rewrite. A member conjured by a claim would work
// until the next restart and then vanish along with the binding pointing at them,
// and the operator would blame enrolment rather than persistence. Refusing at the
// invite, where the fix is one edit away, is the honest place to stop.
func newBinder(cfg *config.Config) (*config.Binder, error) {
	b, err := config.NewBinder(cfg, config.Provisioning{})
	if err != nil {
		return nil, fmt.Errorf("building the enrolment binder: %w", err)
	}
	return b, nil
}

// resolveDeclaredMember finds the member an operator meant by a name.
//
// It accepts the member id as written in kenward.yaml, the name as written there,
// and the name run through the same slug rule the setup wizard uses — so somebody
// who told the wizard "María José" and then typed `kenward invite --name "María
// José"` finds the member the wizard created for them, rather than being told there
// is no such person because of an accent.
func resolveDeclaredMember(cfg *config.Config, name string) (domain.Member, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.Member{}, false
	}
	for _, candidate := range []domain.MemberID{
		domain.MemberID(name),
		domain.MemberID(setup.Slugify(name)),
		enrol.MemberIDFor(name),
	} {
		if candidate == "" {
			continue
		}
		if m, ok := cfg.MemberByID(candidate); ok {
			return m, true
		}
	}
	for _, m := range cfg.DomainMembers() {
		if strings.EqualFold(strings.TrimSpace(m.Name), name) {
			return m, true
		}
	}
	return domain.Member{}, false
}

// undeclaredMemberHelp is what somebody sees when they invite a person the
// configuration does not name.
//
// It names the file and the four fields, because the person reading it has just been
// handed a claim code that will not work and needs to know precisely what to edit —
// not that provisioning was absent, which is true and useless.
func undeclaredMemberHelp(cfg *config.Config, path, name string) string {
	id := setup.Slugify(name)
	if id == "" {
		id = "someone"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "no member in %s matches %q.\n\n", path, name)
	fmt.Fprintf(&b, "kenward will not invent one. Add them to that file first — four fields:\n\n")
	fmt.Fprintf(&b, "  members:\n")
	fmt.Fprintf(&b, "    - id: %s\n", id)
	fmt.Fprintf(&b, "      name: %s\n", name)
	fmt.Fprintf(&b, "      private_space: %s-private\n", id)
	fmt.Fprintf(&b, "      tiers: [%s]\n\n", suggestedTierChain(cfg))
	fmt.Fprintf(&b, "The tier chain is the privacy policy for their private conversations and has no\n")
	fmt.Fprintf(&b, "default: a local-only chain refuses rather than reaching for a provider, and\n")
	fmt.Fprintf(&b, "nothing here will choose it on their behalf. Leave telegram_id out — it arrives\n")
	fmt.Fprintf(&b, "when they claim. Then run this command again.\n")
	if cfg.Mode == config.ModeIsolated {
		fmt.Fprintf(&b, "\nIn isolated mode they also need their own bot_token_env, distinct from every\n")
		fmt.Fprintf(&b, "other member's, and a container of their own.\n")
	}
	return b.String()
}

// suggestedTierChain names a chain the household already has endpoints for,
// preferring one that stays in the house. It is a suggestion in an error message, not
// a default anything applies.
func suggestedTierChain(cfg *config.Config) string {
	local := localTiers(cfg)
	for _, ep := range cfg.Endpoints {
		for _, tag := range ep.Tags {
			if local[tag] {
				return tag
			}
		}
	}
	return setup.LocalTier
}
