package main

import (
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

func inviteStore(cfg *config.Config) *enrol.FileStore {
	return enrol.NewFileStore(filepath.Join(cfg.DataDir, inviteStoreFileName))
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
