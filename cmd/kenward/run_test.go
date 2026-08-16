package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/supervisor"
)

// TestResolveUnitSelection is the precedence table, conflicts included.
//
// Isolated mode has two deployment paths — an operator's compose file passing
// --member, and the host supervisor passing KENWARD_MEMBER to a pod — and both must
// work. Where they disagree, neither wins: a pod quietly serving the wrong member is
// discovered when somebody's private memory turns up in the wrong conversation.
func TestResolveUnitSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		flagMember string
		flagGroup  bool
		vars       map[string]string
		wantMember string
		wantGroup  bool
		wantErr    []string // substrings the error must contain
	}{
		{
			name: "neither: whole household",
		},
		{
			name:       "flag member only",
			flagMember: "david",
			wantMember: "david",
		},
		{
			name:      "flag group only",
			flagGroup: true,
			wantGroup: true,
		},
		{
			name:       "environment member only",
			vars:       map[string]string{supervisor.EnvMember: "david"},
			wantMember: "david",
		},
		{
			name:      "environment group only",
			vars:      map[string]string{supervisor.EnvGroup: "1"},
			wantGroup: true,
		},
		{
			name:       "flag wins where they agree",
			flagMember: "david",
			vars:       map[string]string{supervisor.EnvMember: "david"},
			wantMember: "david",
		},
		{
			name:      "group agrees across both sources",
			flagGroup: true,
			vars:      map[string]string{supervisor.EnvGroup: "1"},
			wantGroup: true,
		},
		{
			name:    "empty environment values are absent, not a selection",
			vars:    map[string]string{supervisor.EnvMember: "", supervisor.EnvGroup: ""},
			wantErr: nil,
		},
		{
			name:      "KENWARD_GROUP=0 is a no",
			vars:      map[string]string{supervisor.EnvGroup: "0"},
			wantGroup: false,
		},

		// --- conflicts: every one of these is a usage error ---
		{
			name:       "both flags",
			flagMember: "david",
			flagGroup:  true,
			wantErr:    []string{"--member david", "--group", "exactly one unit"},
		},
		{
			name:    "both environment variables",
			vars:    map[string]string{supervisor.EnvMember: "david", supervisor.EnvGroup: "1"},
			wantErr: []string{"KENWARD_MEMBER=david", "KENWARD_GROUP", "exactly one unit"},
		},
		{
			name:       "flag names one member, environment names another",
			flagMember: "david",
			vars:       map[string]string{supervisor.EnvMember: "jordan"},
			wantErr:    []string{"disagree", "member david", "member jordan"},
		},
		{
			name:       "flag names a member, environment names the group",
			flagMember: "david",
			vars:       map[string]string{supervisor.EnvGroup: "1"},
			wantErr:    []string{"disagree", "member david", "household group"},
		},
		{
			name:      "flag names the group, environment names a member",
			flagGroup: true,
			vars:      map[string]string{supervisor.EnvMember: "jordan"},
			wantErr:   []string{"disagree", "household group", "member jordan"},
		},
		{
			name:    "KENWARD_GROUP with a member's name is a mistake, not a guess",
			vars:    map[string]string{supervisor.EnvGroup: "david"},
			wantErr: []string{"KENWARD_GROUP", "not a yes or a no"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveUnitSelection(tc.flagMember, tc.flagGroup, lookup(tc.vars))
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("want an error, got selection %+v", got)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error does not mention %q:\n%v", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.member != tc.wantMember || got.group != tc.wantGroup {
				t.Errorf("selection = {member:%q group:%v}, want {member:%q group:%v}",
					got.member, got.group, tc.wantMember, tc.wantGroup)
			}
		})
	}
}

// TestRunRejectsUnitSelectorInSimpleMode, however the selector arrived.
//
// A flag that silently does nothing is how a household comes to believe it is
// isolated when it is one shared process, so this is exit 2 rather than a warning.
func TestRunRejectsUnitSelectorInSimpleMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		vars map[string]string
	}{
		{"--member", []string{"run", "--member", "david"}, nil},
		{"--group", []string{"run", "--group"}, nil},
		{"KENWARD_MEMBER", []string{"run"}, map[string]string{supervisor.EnvMember: "david"}},
		{"KENWARD_GROUP", []string{"run"}, map[string]string{supervisor.EnvGroup: "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vars := fullEnvironment()
			for k, v := range tc.vars {
				vars[k] = v
			}
			h := newHarness(t, simpleYAML, vars)
			h.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
				t.Fatal("a supervisor was built for a configuration that should have been refused")
				return nil, nil
			}
			if code := h.run(tc.args...); code != exitUsage {
				t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
			}
			if !strings.Contains(h.stderr(), "isolated mode only") {
				t.Errorf("stderr does not explain why:\n%s", h.stderr())
			}
		})
	}
}

// TestRunSelectsSupervisorByMode covers the three-way choice `run` makes.
func TestRunSelectsSupervisorByMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		yaml      string
		args      []string
		wantMode  config.Mode
		wantUnit  string
		wantGroup bool
	}{
		{"simple, whole household", simpleYAML, []string{"run"}, config.ModeSimple, "", false},
		{"isolated, host supervisor", isolatedYAML, []string{"run"}, config.ModeIsolated, "", false},
		{"isolated, one member's pod", isolatedYAML, []string{"run", "--member", "david"}, config.ModeIsolated, "david", false},
		{"isolated, the group's pod", isolatedYAML, []string{"run", "--group"}, config.ModeIsolated, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			var gotMode config.Mode
			var gotSel unitSelection
			h.e.supervisors = func(_ *env, cfg *config.Config, opts runOptions, _ *slog.Logger) (supervisor.Supervisor, error) {
				gotMode = cfg.Mode
				gotSel = opts.selection
				return stubSupervisor{}, nil
			}
			if code := h.run(tc.args...); code != exitOK {
				t.Fatalf("exit = %d, want 0\n%s", code, h.both())
			}
			if gotMode != tc.wantMode {
				t.Errorf("mode = %q, want %q", gotMode, tc.wantMode)
			}
			if gotSel.member != tc.wantUnit || gotSel.group != tc.wantGroup {
				t.Errorf("selection = {member:%q group:%v}, want {member:%q group:%v}",
					gotSel.member, gotSel.group, tc.wantUnit, tc.wantGroup)
			}
			h.assertNoSecrets(t)
		})
	}
}

// TestRunRefusesToServeWithoutLore.
//
// Nothing else in the process fails when the program memory.lore_command names is not
// installed. memory.NewClient checks only that the command is non-empty and spawns
// nothing until the first call; a turn that cannot read a space degrades that space
// rather than failing. So a node with no lore starts cleanly, reports itself ready,
// authorises its bot, and then remembers nothing anyone tells it — the failure mode
// the Dockerfile warns about in its first paragraph, arriving in silence.
//
// It is a container's default outcome, not an exotic one: the image deliberately
// carries no lore, and a pod started by `kenward run` in isolated mode cannot be
// bind-mounted one, because keel's sandbox.Spec has no bind-mount. Until this refusal
// existed the supervisor path's own default image produced a household of pods with
// no memory at all.
// The isolated HOST supervisor is exempt, and its case is in the table below: it
// starts pods and holds no memory client of its own, so refusing it would refuse
// every correct isolated household whose lore lives in the pods that use it. That
// was not theoretical — it is what the first real-podman run of this check did.
func TestRunRefusesToServeWithoutLore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		yaml    string
		args    []string
		refused bool
	}{
		{"simple: every unit runs here", simpleYAML, []string{"run"}, true},
		{"isolated: a member's pod", isolatedYAML, []string{"run", "--member", "david"}, true},
		{"isolated: the group's pod", isolatedYAML, []string{"run", "--group"}, true},
		{"isolated: the host supervisor, which never touches lore", isolatedYAML, []string{"run"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			h.e.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
			built := false
			h.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
				built = true
				return stubSupervisor{}, nil
			}
			code := h.run(tc.args...)
			if !tc.refused {
				if code != exitOK || !built {
					t.Fatalf("exit = %d, supervisor built = %v; the host supervisor holds no lore client and must start\n%s",
						code, built, h.both())
				}
				return
			}
			if code != exitFailure {
				t.Fatalf("exit = %d, want %d — a node with no memory must not start\n%s", code, exitFailure, h.both())
			}
			if built {
				t.Error("the supervisor was built anyway; nothing may be served without memory")
			}
			for _, want := range []string{"lore", "PATH", "/usr/local/bin/lore", "--image"} {
				if !strings.Contains(h.stderr(), want) {
					t.Errorf("stderr does not mention %q, so an operator is not told how to fix it:\n%s", want, h.stderr())
				}
			}
		})
	}
}

// TestRunRefusesToServeWhenLoreDoesNotAnswer is the half of the check above that a
// PATH lookup cannot make, and the half that a real container actually hits.
//
// An uninitialised LORE_HOME is the state every fresh volume is in. `lore mcp` exits
// before the MCP handshake against one — "no account at /home/nonroot/.lore/account.json
// (run `lore init`)" — so exec.LookPath succeeds, the guard passes, and the node starts
// into exactly the silence the guard exists to prevent: it authorises, greets, and
// records nothing. Measured on a real container before this test existed.
//
// The exemption is unchanged and is in the table for the same reason it is in the one
// above: the isolated host supervisor holds no memory client, and demanding lore of it
// refused every correct isolated household the first time that was got wrong.
func TestRunRefusesToServeWhenLoreDoesNotAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		yaml    string
		args    []string
		refused bool
	}{
		{"simple: every unit runs here", simpleYAML, []string{"run"}, true},
		{"isolated: a member's pod", isolatedYAML, []string{"run", "--member", "david"}, true},
		{"isolated: the group's pod", isolatedYAML, []string{"run", "--group"}, true},
		{"isolated: the host supervisor, which never touches lore", isolatedYAML, []string{"run"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.yaml, fullEnvironment())
			// lore is on PATH. It just does not answer, which is what an
			// uninitialised LORE_HOME looks like from here.
			h.e.probes = loreUninitialised(healthyProbes())
			built := false
			h.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
				built = true
				return stubSupervisor{}, nil
			}
			code := h.run(tc.args...)
			if !tc.refused {
				if code != exitOK || !built {
					t.Fatalf("exit = %d, supervisor built = %v; the host supervisor holds no lore client and must start\n%s",
						code, built, h.both())
				}
				return
			}
			if code != exitFailure {
				t.Fatalf("exit = %d, want %d — a node whose lore does not answer has no memory\n%s",
					code, exitFailure, h.both())
			}
			if built {
				t.Error("the supervisor was built anyway; nothing may be served without memory")
			}
			// The remedy, not just the fault: an operator reading this has to be told
			// the store needs initialising, which is the one thing a PATH check could
			// never tell them.
			for _, want := range []string{"did not answer", "LORE_HOME", "lore init"} {
				if !strings.Contains(h.stderr(), want) {
					t.Errorf("stderr does not mention %q, so an operator is not told how to fix it:\n%s", want, h.stderr())
				}
			}
		})
	}
}

// TestRunStartsWhenOnlyOneSpaceIsUnknown draws the other edge of the same check.
//
// A space lore does not hold is one space's problem and `doctor`'s to report. Refusing
// the whole household its assistant over a mistyped space id would be a second and
// larger outage than the silent one this check exists to prevent, so lore answering is
// the whole of the question `run` asks.
func TestRunStartsWhenOnlyOneSpaceIsUnknown(t *testing.T) {
	t.Parallel()
	// simpleYAML's spaces are ids, so the harness's healthy probe answers for them;
	// this one refuses every space while lore itself answers.
	h := newHarness(t, simpleYAML, fullEnvironment())
	base := healthyProbes()
	h.e.probes = base
	h.e.probes.lore = func(_ context.Context, cfg *config.Config, scope config.UnitScope) loreResult {
		var res loreResult
		for _, s := range configuredSpaces(cfg, scope) {
			res.Spaces = append(res.Spaces, spaceResult{Space: s, Err: unknownSpaceErr(s)})
		}
		return res
	}
	built := false
	h.e.supervisors = func(*env, *config.Config, runOptions, *slog.Logger) (supervisor.Supervisor, error) {
		built = true
		return stubSupervisor{}, nil
	}
	if code := h.run("run"); code != exitOK || !built {
		t.Fatalf("exit = %d, supervisor built = %v; lore answered, so the household must be served\n%s",
			code, built, h.both())
	}
}

// TestRunRefusesUnknownMember: a pod told to serve somebody the configuration does
// not name has nothing to serve, and starting anyway would look like it worked.
func TestRunRefusesUnknownMember(t *testing.T) {
	t.Parallel()
	h := newHarness(t, isolatedYAML, fullEnvironment())
	if code := h.run("run", "--member", "nobody"); code != exitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, exitUsage, h.both())
	}
	if !strings.Contains(h.stderr(), "names no member") {
		t.Errorf("stderr does not say why:\n%s", h.stderr())
	}
}

// TestStartupSummaryNamesEverySpace.
//
// docs/CLI.md: one summary line per space, naming the mode, the members served and
// the tier chain, so that an operator can read it and know whether a private space
// can reach a provider.
func TestStartupSummaryNamesEverySpace(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, simpleYAML)
	lines := startupSummary(cfg, unitSelection{})

	joined := renderAttrs(lines)
	for _, want := range []string{
		"mode=simple",
		"space=7d5047bb-d939-4539-b3db-8b6221a2e245",
		"space=5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19",
		"space=dac31e70-72e4-4b10-9cef-a6276c4a87b8",
		"members_served=david,jordan",
		"tiers=[local]",
		"tiers=[local, cloud]",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("startup summary does not contain %q:\n%s", want, joined)
		}
	}

	// The point of the line: david's chain is local-only and reaches no provider;
	// jordan's does. Both facts have to be readable off the line itself.
	if !strings.Contains(joined, "space=7d5047bb-d939-4539-b3db-8b6221a2e245") || !strings.Contains(joined, "reaches_provider=false") {
		t.Errorf("david's space does not report reaches_provider=false:\n%s", joined)
	}
	if !strings.Contains(joined, "reaches_provider=true") {
		t.Errorf("no space reports reaches_provider=true, but jordan's chain names cloud:\n%s", joined)
	}
}

// TestStartupSummaryInAPodNamesOnlyThatUnit.
func TestStartupSummaryInAPodNamesOnlyThatUnit(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, isolatedYAML)

	member := renderAttrs(startupSummary(cfg, unitSelection{member: "david"}))
	if !strings.Contains(member, "space=7d5047bb-d939-4539-b3db-8b6221a2e245") {
		t.Errorf("david's pod does not name david's space:\n%s", member)
	}
	for _, unwanted := range []string{"space=5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19", "space=dac31e70-72e4-4b10-9cef-a6276c4a87b8"} {
		if strings.Contains(member, unwanted) {
			t.Errorf("david's pod names %q, which it does not serve:\n%s", unwanted, member)
		}
	}

	group := renderAttrs(startupSummary(cfg, unitSelection{group: true}))
	if !strings.Contains(group, "space=dac31e70-72e4-4b10-9cef-a6276c4a87b8") {
		t.Errorf("the group pod does not name the shared space:\n%s", group)
	}
	// A group scope may never name a private space, and the line an operator reads
	// must not suggest otherwise.
	for _, unwanted := range []string{"7d5047bb-d939-4539-b3db-8b6221a2e245", "5f2a9c14-8e0b-4a77-9d31-c6b40e7f2a19"} {
		if strings.Contains(group, unwanted) {
			t.Errorf("the group pod's summary names %q:\n%s", unwanted, group)
		}
	}
}

// TestStartupSummaryMembersServedIsScopedToTheUnit guards the defect where a member
// pod's members_served named the whole household instead of the one member it runs:
// jordan's pod reported "members_served=david" alongside its own
// topology="this pod runs only member jordan". Every members_served value a pod emits
// must name only what that pod serves.
func TestStartupSummaryMembersServedIsScopedToTheUnit(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, isolatedYAML)

	member := startupSummary(cfg, unitSelection{member: "david"})
	for _, v := range membersServedValues(member) {
		if v != "david" {
			t.Errorf("david's pod reports members_served=%q, want \"david\"\n%s", v, renderAttrs(member))
		}
	}

	group := startupSummary(cfg, unitSelection{group: true})
	for _, v := range membersServedValues(group) {
		if strings.Contains(v, "david") || strings.Contains(v, "jordan") {
			t.Errorf("the group pod reports members_served=%q, naming an individual member\n%s", v, renderAttrs(group))
		}
	}
}

func membersServedValues(lines [][]any) []string {
	var vals []string
	for _, line := range lines {
		for i := 0; i+1 < len(line); i += 2 {
			if line[i] == "members_served" {
				vals = append(vals, line[i+1].(string))
			}
		}
	}
	return vals
}

func renderAttrs(lines [][]any) string {
	var b strings.Builder
	for _, line := range lines {
		for i := 0; i+1 < len(line); i += 2 {
			b.WriteString(strings.TrimSpace(toString(line[i])))
			b.WriteString("=")
			b.WriteString(toString(line[i+1]))
			b.WriteString(" ")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// stubSupervisor stands in for a running household: it starts, it stops, and it
// never touches Telegram, lore or a container runtime.
type stubSupervisor struct{}

func (stubSupervisor) Start(context.Context) error { return nil }
func (stubSupervisor) Stop(context.Context) error  { return nil }
func (stubSupervisor) Health(context.Context) ([]supervisor.UnitHealth, error) {
	return nil, nil
}

func mustLoad(t *testing.T, yaml string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kenward.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path, filepath.Join(dir, "data"), testSecrets(fullEnvironment()))
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	return cfg
}
