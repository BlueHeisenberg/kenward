package privacy

import (
	"strings"
	"testing"
)

// TestDashboardNoteSaysTheTrueThingForEveryReach.
//
// It is in a test file of its own rather than in privacy_test.go so that the statements'
// own golden tests and this one can be edited independently — but the rule is the same
// one, and it is the reason this package exists: nothing here may be more reassuring
// than what the code does.
func TestDashboardNoteSaysTheTrueThingForEveryReach(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		r    Reach
		tls  bool
		// must appear
		want []string
		// must not appear, because it would be a claim this configuration does not
		// earn
		forbidden []string
	}{
		{
			name: "off",
			r:    ReachOff,
			want: []string{"The admin dashboard is off", "No port is open"},
		},
		{
			name: "loopback",
			r:    ReachLoopback,
			want: []string{"this machine only", "http://127.0.0.1:8770"},
			// It must not claim nobody can change anything: whoever can open a
			// browser on this computer can, and that is worth saying.
			forbidden: []string{"nobody"},
		},
		{
			name: "tailnet over http",
			r:    ReachTailnet,
			want: []string{"tailnet or", "already authenticated and\nencrypted", "The connection is http"},
		},
		{
			name: "tailnet over https",
			r:    ReachTailnet,
			tls:  true,
			want: []string{"The connection is https"},
		},
		{
			name: "lan over https",
			r:    ReachLAN,
			tls:  true,
			want: []string{
				"Everyone on your wifi can reach the login page",
				"check the fingerprint",
				"generated and signed itself",
			},
		},
		{
			// kenward refuses to start this way. The statement still has to be
			// true rather than absent, because a blank where a warning belongs is
			// how somebody concludes there is nothing to warn about.
			name: "lan over plain http says so plainly",
			r:    ReachLAN,
			want: []string{"plain HTTP", "in the clear", "not a configuration\nkenward will start with"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DashboardNote(tc.r, "http://127.0.0.1:8770", tc.tls)
			if strings.TrimSpace(got) == "" {
				t.Fatal("no statement at all")
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q from:\n%s", want, got)
				}
			}
			for _, no := range tc.forbidden {
				if strings.Contains(strings.ToLower(got), no) {
					t.Errorf("contains %q, which this configuration does not earn:\n%s", no, got)
				}
			}
		})
	}
}

// TestEveryReachHasAStatement: a Reach with no words is a household told nothing about a
// port that is open.
func TestEveryReachHasAStatement(t *testing.T) {
	t.Parallel()
	for r := ReachOff; r <= ReachLAN; r++ {
		if strings.TrimSpace(DashboardNote(r, "http://x:1", false)) == "" {
			t.Errorf("Reach(%d) has no statement", r)
		}
	}
}

// TestTheStatementNamesTheAddress. An operator reading this has to be able to check it
// against what is actually listening, and a paragraph that says "somewhere" cannot be
// checked against anything.
func TestTheStatementNamesTheAddress(t *testing.T) {
	t.Parallel()
	const addr = "https://192.168.1.20:8770"
	for _, r := range []Reach{ReachLoopback, ReachTailnet, ReachLAN} {
		got := DashboardNote(r, addr, true)
		if !strings.Contains(got, addr) {
			t.Errorf("Reach(%d) does not name %s:\n%s", r, addr, got)
		}
	}
}
