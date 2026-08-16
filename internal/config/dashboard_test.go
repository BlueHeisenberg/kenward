package config

import (
	"strings"
	"testing"
)

// TestDashboardValidation pins the rules that matter, and the two that matter most are
// the refusals: an exposure that contradicts its own bind, and a LAN listener with no
// TLS. Both of those are a household being told something false about who can reach the
// machine holding every member's private memory.
func TestDashboardValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		d    DashboardConfig
		want string // a substring of the expected problem, or "" for no problem
	}{
		{
			name: "absent block is off and fine",
			d:    DashboardConfig{},
		},
		{
			name: "loopback default",
			d:    DashboardConfig{Enabled: true},
		},
		{
			name: "explicit loopback",
			d:    DashboardConfig{Enabled: true, Bind: "127.0.0.1:9000", Exposure: ExposureLoopback},
		},
		{
			name: "localhost by name counts as loopback",
			d:    DashboardConfig{Enabled: true, Bind: "localhost:9000", Exposure: ExposureLoopback},
		},
		{
			name: "loopback claim over a LAN address is refused",
			d:    DashboardConfig{Enabled: true, Bind: "192.168.1.20:8770", Exposure: ExposureLoopback},
			want: "not a loopback address",
		},
		{
			name: "loopback claim over every interface is refused",
			d:    DashboardConfig{Enabled: true, Bind: "0.0.0.0:8770", Exposure: ExposureLoopback},
			want: "not a loopback address",
		},
		{
			name: "a disabled dashboard is still checked",
			d:    DashboardConfig{Bind: "0.0.0.0:8770", Exposure: ExposureLoopback},
			want: "not a loopback address",
		},
		{
			name: "lan without tls is refused",
			d:    DashboardConfig{Enabled: true, Bind: "192.168.1.20:8770", Exposure: ExposureLAN},
			want: "requires TLS",
		},
		{
			name: "lan with tls is fine",
			d: DashboardConfig{Enabled: true, Bind: "192.168.1.20:8770", Exposure: ExposureLAN,
				TLSCertFile: "/x/cert.pem", TLSKeyFile: "/x/key.pem"},
		},
		{
			name: "tailnet on a wildcard is refused",
			d:    DashboardConfig{Enabled: true, Bind: "0.0.0.0:8770", Exposure: ExposureTailnet},
			want: "every interface",
		},
		{
			name: "tailnet on an address is fine",
			d:    DashboardConfig{Enabled: true, Bind: "100.101.102.103:8770", Exposure: ExposureTailnet},
		},
		{
			name: "half a certificate is refused",
			d:    DashboardConfig{Enabled: true, Exposure: ExposureLoopback, TLSCertFile: "/x/cert.pem"},
			want: "must be given together",
		},
		{
			name: "an unknown exposure is refused",
			d:    DashboardConfig{Enabled: true, Exposure: Exposure("everywhere")},
			want: "is not an exposure",
		},
		{
			name: "a bind with no port is refused",
			d:    DashboardConfig{Enabled: true, Bind: "127.0.0.1", Exposure: ExposureLoopback},
			want: "host:port",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Config{Dashboard: tc.d}
			p := &problems{}
			c.validateDashboard(p)
			joined := strings.Join(p.list, "\n")
			switch {
			case tc.want == "" && len(p.list) > 0:
				t.Fatalf("unexpected problems:\n%s", joined)
			case tc.want != "" && !strings.Contains(joined, tc.want):
				t.Fatalf("problems = %q, want one containing %q", joined, tc.want)
			}
		})
	}
}

// TestDashboardSummaryIsTrue: the one line `kenward doctor` and the dashboard both print.
func TestDashboardSummaryIsTrue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		d    DashboardConfig
		want string
	}{
		{DashboardConfig{}, "admin dashboard: off — no port is opened"},
		{DashboardConfig{Enabled: true}, "admin dashboard: loopback on http://127.0.0.1:8770"},
		{
			DashboardConfig{Enabled: true, Bind: "192.168.1.20:8770", Exposure: ExposureLAN,
				TLSCertFile: "c", TLSKeyFile: "k"},
			"admin dashboard: lan on https://192.168.1.20:8770",
		},
	} {
		if got := tc.d.DashboardSummary(); got != tc.want {
			t.Errorf("DashboardSummary() = %q, want %q", got, tc.want)
		}
	}
}

// TestDashboardBlockRoundTrips through the real decoder, unknown fields and all.
func TestDashboardBlockRoundTrips(t *testing.T) {
	t.Parallel()
	const doc = `
mode: simple
dashboard:
  enabled: true
  bind: 100.64.1.2:9999
  exposure: tailnet
`
	cfg, err := Decode(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Dashboard.Enabled {
		t.Error("enabled did not decode")
	}
	if cfg.Dashboard.BindAddr() != "100.64.1.2:9999" {
		t.Errorf("bind = %q", cfg.Dashboard.BindAddr())
	}
	if cfg.Dashboard.ExposureOrDefault() != ExposureTailnet {
		t.Errorf("exposure = %q", cfg.Dashboard.ExposureOrDefault())
	}
	if cfg.Dashboard.Loopback() {
		t.Error("a tailnet address is reported as loopback")
	}
}
