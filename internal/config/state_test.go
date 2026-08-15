package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

func claimedAt() time.Time { return time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC) }

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	st := config.NewState()
	st.Bind("david", 12345678, claimedAt())
	st.Bind("maria", 87654321, claimedAt().Add(time.Hour))
	if err := st.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := config.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	b, ok := got.Binding("david")
	if !ok || b.TelegramID != 12345678 || !b.EnrolledAt.Equal(claimedAt()) {
		t.Errorf("Binding(david) = (%+v, %v)", b, ok)
	}
	if _, ok := got.Binding("nobody"); ok {
		t.Error("Binding(nobody) matched")
	}
	if got.Version == 0 {
		t.Error("Version was not written")
	}
}

// TestLoadStateOfAHouseholdThatHasEnrolledNobody: a missing file is first run, not a
// failure. Refusing to start over it would make enrolment impossible to reach.
func TestLoadStateOfAHouseholdThatHasEnrolledNobody(t *testing.T) {
	st, err := config.LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadState() on a missing file: %v", err)
	}
	if len(st.Bindings) != 0 {
		t.Errorf("Bindings = %v, want empty", st.Bindings)
	}
	// And it must be usable straight away, without a nil map panic.
	st.Bind("david", 42, claimedAt())
	if b, ok := st.Binding("david"); !ok || b.TelegramID != 42 {
		t.Errorf("Binding(david) = (%+v, %v)", b, ok)
	}
}

// TestLoadStateRefusesToGuess: a corrupt file is an error, because guessing at enrolment
// state means guessing at who may talk to the node.
func TestLoadStateRefusesToGuess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if _, err := config.LoadState(path); err == nil {
		t.Fatal("LoadState() on a corrupt file = nil, want an error")
	} else if !strings.Contains(err.Error(), "state.json") {
		t.Errorf("error does not name the file: %v", err)
	}
}

func TestStateSaveCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "state.json")
	st := config.NewState()
	st.Bind("david", 1, claimedAt())
	if err := st.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file was not created: %v", err)
	}
}

// TestStateSaveIsAtomic checks the two things the write-temp-rename dance buys: no
// temporary files are left behind, and an existing file is replaced rather than
// appended to or truncated in place.
func TestStateSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	first := config.NewState()
	first.Bind("david", 1, claimedAt())
	if err := first.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	second := config.NewState()
	second.Bind("maria", 2, claimedAt())
	if err := second.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only state.json", names)
	}

	got, err := config.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if _, ok := got.Binding("david"); ok {
		t.Error("the replaced file still holds the previous contents")
	}
	if _, ok := got.Binding("maria"); !ok {
		t.Error("the replaced file does not hold the new contents")
	}
}

// TestStateFilePermissions: the file records who may talk to this household, so other
// users on the machine have no business reading it.
func TestStateFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := config.NewState().Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestStateBindAndUnbind(t *testing.T) {
	st := config.NewState()
	st.Bind("david", 111, claimedAt())
	st.Bind("maria", 222, claimedAt())

	if id, ok := st.MemberByTelegramID(222); !ok || id != "maria" {
		t.Errorf("MemberByTelegramID(222) = (%q, %v)", id, ok)
	}
	if _, ok := st.MemberByTelegramID(0); ok {
		t.Error("MemberByTelegramID(0) matched; zero is never a binding")
	}
	if _, ok := st.MemberByTelegramID(333); ok {
		t.Error("MemberByTelegramID(333) matched an unbound account")
	}

	// Re-claiming from a new account replaces the binding rather than adding one.
	st.Bind("david", 999, claimedAt().Add(24*time.Hour))
	if _, ok := st.MemberByTelegramID(111); ok {
		t.Error("the old account is still bound after a re-claim")
	}
	if id, ok := st.MemberByTelegramID(999); !ok || id != "david" {
		t.Errorf("MemberByTelegramID(999) = (%q, %v)", id, ok)
	}

	st.Unbind("david")
	if _, ok := st.Binding("david"); ok {
		t.Error("Unbind() left the binding in place")
	}
	if _, ok := st.Binding("maria"); !ok {
		t.Error("Unbind() removed someone else's binding")
	}
}

const mergeYAML = `
mode: simple
household: {name: Home, shared_space: household, tiers: [local]}
telegram: {bot_token_env: T}
members:
  - {id: david, name: David, private_space: david-private, tiers: [local]}
  - {id: maria, name: Maria, telegram_id: 222, private_space: maria-private, tiers: [local]}
endpoints:
  - {name: monster, base_url: http://m:1/v1, model: q, tags: [local]}
`

func TestMergeStateFillsInBindings(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader(mergeYAML))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}

	st := config.NewState()
	st.Bind("david", 111, claimedAt())
	st.Bind("maria", 222, claimedAt()) // agrees with the file
	st.Bind("ghost", 333, claimedAt()) // a member who has since been deleted

	if err := cfg.MergeState(st); err != nil {
		t.Fatalf("MergeState() error: %v", err)
	}

	members := cfg.DomainMembers()
	if members[0].TelegramID != 111 || !members[0].EnrolledAt.Equal(claimedAt()) {
		t.Errorf("david = %+v, want the binding folded in", members[0])
	}
	if !members[0].Enrolled() {
		t.Error("david is not reported as enrolled after the merge")
	}
	if members[1].TelegramID != 222 || !members[1].EnrolledAt.Equal(claimedAt()) {
		t.Errorf("maria = %+v, want an agreeing binding to be accepted", members[1])
	}
	if len(members) != 2 {
		t.Errorf("a binding for a deleted member added one back: %+v", members)
	}
	// The merged configuration is the one everything downstream reads.
	if m, ok := cfg.MemberByTelegramID(111); !ok || m.ID != "david" {
		t.Errorf("MemberByTelegramID(111) = (%+v, %v) after the merge", m, ok)
	}
}

// TestMergeStateConflict: the file and the state naming different accounts for one
// member means someone hand-edited against an existing binding. Preferring either one
// silently would lock a member out or hand their space to the wrong account.
func TestMergeStateConflict(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader(mergeYAML))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	st := config.NewState()
	st.Bind("maria", 999, claimedAt())

	err = cfg.MergeState(st)
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("MergeState() error = %v (%T), want *config.ValidationError", err, err)
	}
	if !containsSub(ve.Problems, "the file says 222 but state.json records 999") {
		t.Errorf("problem does not explain the conflict: %v", ve.Problems)
	}
	// And it must not have picked a side.
	if cfg.Members[1].TelegramID != 222 {
		t.Errorf("a conflicting merge overwrote the file's value: %d", cfg.Members[1].TelegramID)
	}
}

func TestMergeStateWithNoState(t *testing.T) {
	cfg, err := config.Decode(strings.NewReader(mergeYAML))
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if err := cfg.MergeState(nil); err != nil {
		t.Fatalf("MergeState(nil) error: %v", err)
	}
	if err := cfg.MergeState(config.NewState()); err != nil {
		t.Fatalf("MergeState(empty) error: %v", err)
	}
	if cfg.Members[0].TelegramID != 0 {
		t.Errorf("an unclaimed member gained an id from nowhere: %d", cfg.Members[0].TelegramID)
	}
}

// TestLoadMergesState is the end-to-end path enrolment depends on: a member claims an
// invite, the binding is saved, and the next start serves them without the operator
// touching the YAML.
func TestLoadMergesState(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(cfgPath, []byte(withDataDir(mergeYAML, dir)), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	lookup := env(map[string]string{"T": "token"})

	// First start: nobody has claimed, and the file the operator wrote is untouched.
	cfg, err := config.LoadWithEnv(cfgPath, lookup)
	if err != nil {
		t.Fatalf("LoadWithEnv() error: %v", err)
	}
	if cfg.Members[0].TelegramID != 0 {
		t.Fatalf("david is bound before claiming: %d", cfg.Members[0].TelegramID)
	}

	// A claim: enrol binds and saves.
	st, err := config.LoadState(cfg.StatePath())
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	st.Bind("david", 111, claimedAt())
	if err := st.Save(cfg.StatePath()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Next start: served, with the YAML unchanged on disk.
	cfg, err = config.LoadWithEnv(cfgPath, lookup)
	if err != nil {
		t.Fatalf("LoadWithEnv() error: %v", err)
	}
	m, ok := cfg.MemberByTelegramID(111)
	if !ok || m.ID != "david" || !m.EnrolledAt.Equal(claimedAt()) {
		t.Errorf("MemberByTelegramID(111) = (%+v, %v)", m, ok)
	}

	onDisk, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(onDisk), "- {id: david, name: David, private_space: david-private, tiers: [local]}") {
		t.Error("enrolment rewrote the operator's configuration file")
	}
}

// TestLoadReportsConflictsAlongsideEverythingElse: merge problems and validation
// problems reach the operator as one list, like every other problem does.
func TestLoadReportsConflictsAlongsideEverythingElse(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kenward.yaml")
	broken := strings.Replace(mergeYAML, "tiers: [local]}\n  - {id: maria", "tiers: [gpu]}\n  - {id: maria", 1)
	if err := os.WriteFile(cfgPath, []byte(withDataDir(broken, dir)), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	st := config.NewState()
	st.Bind("maria", 999, claimedAt())
	if err := st.Save(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	_, err := config.LoadWithEnv(cfgPath, env(map[string]string{"T": "t"}))
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("LoadWithEnv() error = %v (%T), want *config.ValidationError", err, err)
	}
	if !containsSub(ve.Problems, "state.json records 999") {
		t.Errorf("the merge conflict is missing: %v", ve.Problems)
	}
	if !containsSub(ve.Problems, `tier "gpu" is not a tag on any endpoint`) {
		t.Errorf("the validation problem is missing: %v", ve.Problems)
	}
}

func TestLoadRefusesACorruptStateFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(cfgPath, []byte(withDataDir(mergeYAML, dir)), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if _, err := config.LoadWithEnv(cfgPath, env(map[string]string{"T": "t"})); err == nil {
		t.Fatal("LoadWithEnv() = nil, want a refusal to start on unreadable enrolment state")
	}
}

// TestEnrolledAtIsNotAConfigurationField: when someone claimed their invite is
// something that happened, not something an operator declares.
func TestEnrolledAtIsNotAConfigurationField(t *testing.T) {
	const doc = `
members:
  - {id: david, private_space: dp, enrolled_at: "2026-08-15T09:30:00Z"}
`
	_, err := config.Decode(strings.NewReader(doc))
	if err == nil {
		t.Fatal("Decode() error = nil, want an unknown-field error for enrolled_at")
	}
	if !strings.Contains(err.Error(), "enrolled_at") {
		t.Errorf("error does not name the field: %v", err)
	}
}

func TestDefaultDataDirIsPerOS(t *testing.T) {
	got := config.DefaultDataDir()
	if got == "" {
		t.Fatal("DefaultDataDir() = \"\"")
	}
	if filepath.Base(got) != "kenward" && filepath.Base(got) != ".kenward" {
		t.Errorf("DefaultDataDir() = %q, want a kenward-named directory", got)
	}

	switch runtime.GOOS {
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" && !strings.HasPrefix(got, local) {
			t.Errorf("DefaultDataDir() = %q, want it under LOCALAPPDATA %q", got, local)
		}
	case "darwin":
		if !strings.Contains(got, filepath.Join("Library", "Application Support")) {
			t.Errorf("DefaultDataDir() = %q, want it under Application Support", got)
		}
	default:
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			if !strings.HasPrefix(got, x) {
				t.Errorf("DefaultDataDir() = %q, want it under XDG_STATE_HOME %q", got, x)
			}
		} else if !strings.Contains(got, filepath.Join(".local", "state")) {
			t.Errorf("DefaultDataDir() = %q, want it under ~/.local/state", got)
		}
	}
}

// TestStateHoldsBindingsAndNothingElse pins the shape of the file on disk. It is state,
// not configuration, and the moment anything else lands in it the split stops meaning
// anything.
func TestStateHoldsBindingsAndNothingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := config.NewState()
	st.Bind("david", 111, claimedAt())
	if err := st.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	written := string(data)
	for _, want := range []string{`"version"`, `"bindings"`, `"david"`, `"telegram_id": 111`, `"enrolled_at"`} {
		if !strings.Contains(written, want) {
			t.Errorf("state file is missing %s:\n%s", want, written)
		}
	}
	for _, unwanted := range []string{"private_space", "tiers", "bot_token", "shared_space", "api_key"} {
		if strings.Contains(written, unwanted) {
			t.Errorf("state file contains configuration (%s):\n%s", unwanted, written)
		}
	}
}

func TestStateKeyIsTheMemberIDNotTheTelegramID(t *testing.T) {
	// Re-claiming from a different Telegram account must keep the same member, and so
	// the same private space. Keying on the Telegram id would make them a new person.
	st := config.NewState()
	st.Bind("david", 111, claimedAt())
	st.Bind("david", 222, claimedAt().Add(time.Hour))

	if len(st.Bindings) != 1 {
		t.Fatalf("Bindings = %v, want one entry per member", st.Bindings)
	}
	var id domain.MemberID = "david"
	if b, ok := st.Binding(id); !ok || b.TelegramID != 222 {
		t.Errorf("Binding(david) = (%+v, %v)", b, ok)
	}
}
