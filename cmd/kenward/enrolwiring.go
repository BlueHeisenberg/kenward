package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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

func inviteStore(cfg *config.Config) *enrol.FileStore {
	return enrol.NewFileStore(filepath.Join(cfg.DataDir, inviteStoreFileName))
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
