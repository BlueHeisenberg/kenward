package config_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
)

// The whole point of the type: it is what enrolment asks for. The interface is
// satisfied structurally and the import edge exists only here, in a test — the
// configuration package itself knows nothing about claim codes.
var _ enrol.Binder = (*config.Binder)(nil)

// localOnly is a tier chain that names no provider, which is the chain a household
// that cares would provision a new member with.
var localOnly = []string{"local"}

func binderHousehold(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Mode:    config.ModeSimple,
		DataDir: t.TempDir(),
		Household: config.HouseholdConfig{
			Name:        "Perez",
			SharedSpace: "household",
			Tiers:       []string{"local", "cloud"},
		},
		Members: []config.MemberConfig{
			{ID: "david", Name: "David", PrivateSpace: "david-private", Tiers: []string{"local"}},
			{ID: "maria", Name: "Maria", PrivateSpace: "maria-private", Tiers: []string{"local", "cloud"}},
		},
	}
}

func newBinder(t *testing.T, cfg *config.Config, p config.Provisioning) *config.Binder {
	t.Helper()
	b, err := config.NewBinder(cfg, p)
	if err != nil {
		t.Fatalf("NewBinder() error: %v", err)
	}
	return b
}

// recorded reads the bindings back through the same loader kenward starts with, so a
// test asserting persistence asserts on the file rather than on the Binder's memory.
func recorded(t *testing.T, cfg *config.Config) map[domain.MemberID]config.Binding {
	t.Helper()
	st, err := config.LoadState(cfg.StatePath())
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	return st.Bindings
}

func stateBytes(t *testing.T, cfg *config.Config) []byte {
	t.Helper()
	data, err := os.ReadFile(cfg.StatePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading the state file: %v", err)
	}
	return data
}

func TestBindConfiguredMember(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{})

	m, err := b.Bind(ctx, "david", "Dave from the invite", 12345678, claimedAt())
	if err != nil {
		t.Fatalf("Bind() error: %v", err)
	}
	if m.ID != "david" || m.TelegramID != 12345678 || !m.EnrolledAt.Equal(claimedAt()) {
		t.Errorf("Bind() = %+v", m)
	}
	// The configuration declared this member, so the configuration's name, space and
	// tier chain are what they get; the invite is a way in, not a way to rename or
	// re-scope somebody.
	if m.Name != "David" || m.Private != "david-private" {
		t.Errorf("Bind() = %+v, want the configured name and space", m)
	}
	if len(m.Tiers) != 1 || m.Tiers[0] != "local" {
		t.Errorf("Tiers = %v, want the configured chain", m.Tiers)
	}
	if !m.Enrolled() {
		t.Error("bound member does not report as enrolled")
	}

	if bd, ok := recorded(t, cfg)["david"]; !ok || bd.TelegramID != 12345678 {
		t.Errorf("state file records (%+v, %v)", bd, ok)
	}

	// The caller's Config is read concurrently and without a lock by everything else
	// in the process. The Binder does not touch it.
	if cfg.Members[0].TelegramID != 0 || !cfg.Members[0].EnrolledAt.IsZero() {
		t.Errorf("Bind() mutated the caller's Config: %+v", cfg.Members[0])
	}
}

// TestBindCreatesAMemberTheConfigurationDoesNotDeclare covers the creation path: the
// member gets the invited name, the wizard's private-space convention and exactly the
// provisioned tier chain — never the household's, which would widen their privacy
// policy without anyone saying so.
func TestBindCreatesAMemberTheConfigurationDoesNotDeclare(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{Tiers: localOnly})

	m, err := b.Bind(ctx, "guest", "  Guest  ", 42, claimedAt())
	if err != nil {
		t.Fatalf("Bind() error: %v", err)
	}
	if m.ID != "guest" || m.Name != "Guest" {
		t.Errorf("Bind() = %+v, want the invited name", m)
	}
	if m.Private != "guest-private" {
		t.Errorf("Private = %q, want guest-private", m.Private)
	}
	if len(m.Tiers) != 1 || m.Tiers[0] != "local" {
		t.Errorf("Tiers = %v, want the provisioned chain", m.Tiers)
	}
	if m.BotTokenEnv != "" {
		t.Errorf("BotTokenEnv = %q; a created member has no token of their own", m.BotTokenEnv)
	}

	members := b.Members()
	if len(members) != 3 || members[2].ID != "guest" {
		t.Errorf("Members() = %+v, want the created member appended", members)
	}
	if _, ok := recorded(t, cfg)["guest"]; !ok {
		t.Error("the created member's binding was not written")
	}

	// The chain is the privacy policy: a caller holding a returned member must not be
	// able to widen the Binder's copy of it in place.
	m.Tiers[0] = "cloud"
	again, _ := b.Member("guest")
	if again.Tiers[0] != "local" {
		t.Errorf("the returned tier chain aliased the Binder's: %v", again.Tiers)
	}
}

// TestBindRefusesToCreateWithoutProvisioning: a member conjured by a claim with an
// unstated tier chain is a member nobody decided the privacy policy for. Refusing is
// the only answer that is not a silent default.
func TestBindRefusesToCreateWithoutProvisioning(t *testing.T) {
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{})

	if _, err := b.Bind(context.Background(), "guest", "Guest", 42, claimedAt()); !errors.Is(err, config.ErrNoProvisioning) {
		t.Fatalf("Bind(unknown) error = %v, want ErrNoProvisioning", err)
	}
	if data := stateBytes(t, cfg); len(data) != 0 {
		t.Errorf("a refused claim wrote a state file: %s", data)
	}
	if _, ok := b.Member("guest"); ok {
		t.Error("a refused claim left a member behind")
	}
}

func TestBindCreationSpaceRules(t *testing.T) {
	ctx := context.Background()

	t.Run("moves out of the shared space", func(t *testing.T) {
		cfg := binderHousehold(t)
		cfg.Household.SharedSpace = "guest-private"
		b := newBinder(t, cfg, config.Provisioning{Tiers: localOnly})

		m, err := b.Bind(ctx, "guest", "Guest", 42, claimedAt())
		if err != nil {
			t.Fatalf("Bind() error: %v", err)
		}
		if m.Private != "guest-private-own" {
			t.Errorf("Private = %q; a private space that is the shared space publishes everything they say", m.Private)
		}
	})

	t.Run("refuses another member's space", func(t *testing.T) {
		cfg := binderHousehold(t)
		cfg.Members[1].PrivateSpace = "guest-private"
		b := newBinder(t, cfg, config.Provisioning{Tiers: localOnly})

		if _, err := b.Bind(ctx, "guest", "Guest", 42, claimedAt()); !errors.Is(err, config.ErrPrivateSpaceTaken) {
			t.Fatalf("Bind() error = %v, want ErrPrivateSpaceTaken", err)
		}
		if data := stateBytes(t, cfg); len(data) != 0 {
			t.Errorf("a refused claim wrote a state file: %s", data)
		}
	})
}

// TestBindIsIdempotent: a retried claim must not fail, and must not restate when the
// member enrolled.
func TestBindIsIdempotent(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{})

	first, err := b.Bind(ctx, "david", "David", 12345678, claimedAt())
	if err != nil {
		t.Fatalf("Bind() error: %v", err)
	}
	before := stateBytes(t, cfg)

	second, err := b.Bind(ctx, "david", "David", 12345678, claimedAt().Add(time.Hour))
	if err != nil {
		t.Fatalf("Bind() again error: %v", err)
	}
	if !second.EnrolledAt.Equal(first.EnrolledAt) {
		t.Errorf("EnrolledAt moved on a retry: %v then %v", first.EnrolledAt, second.EnrolledAt)
	}
	if got := stateBytes(t, cfg); string(got) != string(before) {
		t.Errorf("a retried claim rewrote the state file:\n%s\n%s", before, got)
	}
}

// TestBindRejectsAnAccountThatBelongsToSomebodyElse is the hijack case: moving the
// binding would hand the second claimant the first member's private space.
func TestBindRejectsAnAccountThatBelongsToSomebodyElse(t *testing.T) {
	ctx := context.Background()

	t.Run("bound by a claim", func(t *testing.T) {
		cfg := binderHousehold(t)
		b := newBinder(t, cfg, config.Provisioning{Tiers: localOnly})
		if _, err := b.Bind(ctx, "david", "David", 12345678, claimedAt()); err != nil {
			t.Fatalf("Bind() error: %v", err)
		}
		before := stateBytes(t, cfg)

		for _, id := range []domain.MemberID{"maria", "guest"} {
			_, err := b.Bind(ctx, id, "Someone", 12345678, claimedAt().Add(time.Hour))
			if !errors.Is(err, config.ErrTelegramIDBound) {
				t.Fatalf("Bind(%q) error = %v, want ErrTelegramIDBound", id, err)
			}
			if _, ok := b.Member(id); ok && id == "guest" {
				t.Errorf("the refused claim created member %q", id)
			}
			if m, ok := b.Member(id); ok && m.Enrolled() {
				t.Errorf("member %q was enrolled by a refused claim", id)
			}
		}
		if got := stateBytes(t, cfg); string(got) != string(before) {
			t.Errorf("a refused claim wrote to the state file:\n%s\n%s", before, got)
		}
		if m, _ := b.Member("david"); m.TelegramID != 12345678 {
			t.Errorf("the first member's binding moved: %+v", m)
		}
	})

	// A telegram_id written in the file by hand is a binding too, even though no claim
	// ever produced it.
	t.Run("written in the configuration", func(t *testing.T) {
		cfg := binderHousehold(t)
		cfg.Members[0].TelegramID = 999
		b := newBinder(t, cfg, config.Provisioning{})

		if _, err := b.Bind(ctx, "maria", "Maria", 999, claimedAt()); !errors.Is(err, config.ErrTelegramIDBound) {
			t.Fatalf("Bind() error = %v, want ErrTelegramIDBound", err)
		}
	})
}

// TestBindRebindsAMemberToANewAccount: bindings are keyed by member id precisely so a
// member who changes Telegram accounts stays the same person with the same memory.
func TestBindRebindsAMemberToANewAccount(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{})

	if _, err := b.Bind(ctx, "david", "David", 111, claimedAt()); err != nil {
		t.Fatalf("Bind() error: %v", err)
	}
	m, err := b.Bind(ctx, "david", "David", 222, claimedAt().Add(time.Hour))
	if err != nil {
		t.Fatalf("Bind() to a new account error: %v", err)
	}
	if m.TelegramID != 222 {
		t.Errorf("TelegramID = %d, want 222", m.TelegramID)
	}
	bindings := recorded(t, cfg)
	if len(bindings) != 1 || bindings["david"].TelegramID != 222 {
		t.Errorf("state file = %+v, want one binding to 222", bindings)
	}
}

func TestBindRejectsNonsense(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{Tiers: localOnly})

	tests := []struct {
		name       string
		id         domain.MemberID
		memberName string
		telegramID int64
		want       error
	}{
		{"no member id", "", "Guest", 42, config.ErrNoMemberID},
		{"no telegram id", "david", "David", 0, config.ErrNoTelegramID},
		{"creating without a name", "guest", "   ", 42, config.ErrNoMemberName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := b.Bind(ctx, tt.id, tt.memberName, tt.telegramID, claimedAt()); !errors.Is(err, tt.want) {
				t.Fatalf("Bind() error = %v, want %v", err, tt.want)
			}
			if data := stateBytes(t, cfg); len(data) != 0 {
				t.Errorf("a refused claim wrote a state file: %s", data)
			}
		})
	}
}

func TestUnbind(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{})
	if _, err := b.Bind(ctx, "david", "David", 12345678, claimedAt()); err != nil {
		t.Fatalf("Bind() error: %v", err)
	}

	before, err := b.Unbind(ctx, "david")
	if err != nil {
		t.Fatalf("Unbind() error: %v", err)
	}
	// "As it was": revocation has to be able to name the account it cut off and the
	// space whose key still opens it.
	if before.TelegramID != 12345678 || before.Private != "david-private" || !before.EnrolledAt.Equal(claimedAt()) {
		t.Errorf("Unbind() = %+v, want the member as it was", before)
	}

	now, _ := b.Member("david")
	if now.Enrolled() || !now.EnrolledAt.IsZero() {
		t.Errorf("member is still bound: %+v", now)
	}
	if _, ok := recorded(t, cfg)["david"]; ok {
		t.Error("the binding is still in the state file")
	}
}

// TestUnbindUnknownMember: the error has to be the one the enrolment package tests
// for, without this package importing it.
func TestUnbindUnknownMember(t *testing.T) {
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{})

	_, err := b.Unbind(context.Background(), "nobody")
	if !errors.Is(err, config.ErrUnknownMember) {
		t.Errorf("Unbind(unknown) error = %v, want config.ErrUnknownMember", err)
	}
	if !errors.Is(err, enrol.ErrUnknownMember) {
		t.Errorf("Unbind(unknown) error = %v, want it to match enrol.ErrUnknownMember", err)
	}
	if data := stateBytes(t, cfg); len(data) != 0 {
		t.Errorf("Unbind(unknown) wrote a state file: %s", data)
	}
}

// TestUnbindAMemberWhoNeverClaimed is not an error and not a write: there is nothing
// to clear.
func TestUnbindAMemberWhoNeverClaimed(t *testing.T) {
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{})

	m, err := b.Unbind(context.Background(), "maria")
	if err != nil {
		t.Fatalf("Unbind() error: %v", err)
	}
	if m.ID != "maria" || m.Enrolled() {
		t.Errorf("Unbind() = %+v", m)
	}
	if data := stateBytes(t, cfg); len(data) != 0 {
		t.Errorf("Unbind() of an unenrolled member wrote a state file: %s", data)
	}
}

// TestBindingsSurviveAReload checks the binding through the loader the node actually
// starts with, not just through a second Binder.
func TestBindingsSurviveAReload(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{Tiers: localOnly})
	if _, err := b.Bind(ctx, "david", "David", 12345678, claimedAt()); err != nil {
		t.Fatalf("Bind() error: %v", err)
	}

	reloaded := binderHousehold(t)
	reloaded.DataDir = cfg.DataDir
	again := newBinder(t, reloaded, config.Provisioning{Tiers: localOnly})
	m, ok := again.Member("david")
	if !ok || m.TelegramID != 12345678 || !m.EnrolledAt.Equal(claimedAt()) {
		t.Errorf("after a reload, Member(david) = (%+v, %v)", m, ok)
	}

	// And the same file merges into a configuration the ordinary way.
	st, err := config.LoadState(reloaded.StatePath())
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	fresh := binderHousehold(t)
	fresh.DataDir = cfg.DataDir
	if err := fresh.MergeState(st); err != nil {
		t.Fatalf("MergeState() error: %v", err)
	}
	if fresh.Members[0].TelegramID != 12345678 {
		t.Errorf("MergeState did not see the binding: %+v", fresh.Members[0])
	}
}

// TestNewBinderRefusesToChooseBetweenTheFileAndTheState mirrors MergeState: a
// hand-edited telegram_id that disagrees with a recorded binding is somebody's mistake,
// and guessing means guessing at who may talk to the node.
func TestNewBinderRefusesToChooseBetweenTheFileAndTheState(t *testing.T) {
	cfg := binderHousehold(t)
	st := config.NewState()
	st.Bind("david", 222, claimedAt())
	if err := st.Save(cfg.StatePath()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	cfg.Members[0].TelegramID = 111

	_, err := config.NewBinder(cfg, config.Provisioning{})
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("NewBinder() error = %v, want a *ValidationError", err)
	}
	if !containsSub(ve.Problems, "telegram_id") {
		t.Errorf("problems do not name the field: %v", ve.Problems)
	}
}

// TestFailedSaveLeavesTheHouseholdAlone: the file is what the household is. If it
// could not be written, the Binder must still describe the file that is there.
func TestFailedSaveLeavesTheHouseholdAlone(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{Tiers: localOnly})
	if _, err := b.Bind(ctx, "david", "David", 111, claimedAt()); err != nil {
		t.Fatalf("Bind() error: %v", err)
	}
	saved := stateBytes(t, cfg)

	// A directory where the state file goes: the atomic rename cannot land.
	path := cfg.StatePath()
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the state file: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("creating the obstruction: %v", err)
	}

	if _, err := b.Bind(ctx, "maria", "Maria", 222, claimedAt()); err == nil {
		t.Fatal("Bind() over an unwritable state file = nil, want an error")
	}
	if m, _ := b.Member("maria"); m.Enrolled() {
		t.Errorf("a failed save left maria bound in memory: %+v", m)
	}
	if _, err := b.Bind(ctx, "guest", "Guest", 333, claimedAt()); err == nil {
		t.Fatal("Bind() creating a member over an unwritable state file = nil, want an error")
	}
	if _, ok := b.Member("guest"); ok {
		t.Error("a failed save left a created member behind")
	}
	if _, err := b.Unbind(ctx, "david"); err == nil {
		t.Fatal("Unbind() over an unwritable state file = nil, want an error")
	}
	if m, _ := b.Member("david"); m.TelegramID != 111 {
		t.Errorf("a failed save cleared david in memory: %+v", m)
	}

	// And once the obstruction is gone, the same Binder writes the state it still
	// holds rather than one it half-applied.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the obstruction: %v", err)
	}
	if _, err := b.Bind(ctx, "maria", "Maria", 222, claimedAt()); err != nil {
		t.Fatalf("Bind() after the obstruction went: %v", err)
	}
	bindings := recorded(t, cfg)
	if len(bindings) != 2 || bindings["david"].TelegramID != 111 || bindings["maria"].TelegramID != 222 {
		t.Errorf("state file = %+v, want david and maria\nbefore the failure it was %s", bindings, saved)
	}
}

// TestConcurrentBinds is the one that matters under -race: enrolment is driven from
// the transport's goroutines, and several people can claim at once.
func TestConcurrentBinds(t *testing.T) {
	ctx := context.Background()
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{Tiers: localOnly})

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := domain.MemberID(fmt.Sprintf("guest-%02d", i))
			_, errs[i] = b.Bind(ctx, id, fmt.Sprintf("Guest %d", i), int64(1000+i), claimedAt())
			// Read paths run alongside the writes, because they will in the node.
			b.Member("david")
			b.Members()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Bind(%d) error: %v", i, err)
		}
	}
	if got := len(b.Members()); got != n+2 {
		t.Errorf("Members() = %d, want %d", got, n+2)
	}
	bindings := recorded(t, cfg)
	if len(bindings) != n {
		t.Errorf("state file holds %d bindings, want %d", len(bindings), n)
	}
	for i := range n {
		id := domain.MemberID(fmt.Sprintf("guest-%02d", i))
		if bindings[id].TelegramID != int64(1000+i) {
			t.Errorf("binding for %q = %+v", id, bindings[id])
		}
	}
}

// TestBinderHonoursACancelledContext: a save can block on a filesystem, which is what
// makes these context-taking calls in the first place.
func TestBinderHonoursACancelledContext(t *testing.T) {
	cfg := binderHousehold(t)
	b := newBinder(t, cfg, config.Provisioning{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := b.Bind(ctx, "david", "David", 111, claimedAt()); !errors.Is(err, context.Canceled) {
		t.Errorf("Bind() error = %v, want context.Canceled", err)
	}
	if _, err := b.Unbind(ctx, "david"); !errors.Is(err, context.Canceled) {
		t.Errorf("Unbind() error = %v, want context.Canceled", err)
	}
	if data := stateBytes(t, cfg); len(data) != 0 {
		t.Errorf("a cancelled call wrote a state file: %s", data)
	}
}
