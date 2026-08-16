package setup

import (
	"github.com/BlueHeisenberg/kenward/internal/config"
)

// dashboardDoc is the admin dashboard block as it is written into kenward.yaml.
//
// It mirrors config.DashboardConfig for the reason every other doc type in this package
// mirrors its configuration type: omitempty. A household on loopback should not find
// two empty TLS paths in its file inviting it to guess what they are for.
type dashboardDoc struct {
	Enabled     bool            `yaml:"enabled"`
	Bind        string          `yaml:"bind"`
	Exposure    config.Exposure `yaml:"exposure"`
	TLSCertFile string          `yaml:"tls_cert_file,omitempty"`
	TLSKeyFile  string          `yaml:"tls_key_file,omitempty"`
}

// dashboardDocFor projects the dashboard block, or nothing at all for a household that
// has never turned it on. Absent means off, so writing the block for a disabled
// dashboard would only add a switch nobody has read the consequences of.
func dashboardDocFor(d config.DashboardConfig) *dashboardDoc {
	if !d.Enabled {
		return nil
	}
	return &dashboardDoc{
		Enabled:     true,
		Bind:        d.BindAddr(),
		Exposure:    d.ExposureOrDefault(),
		TLSCertFile: d.TLSCertFile,
		TLSKeyFile:  d.TLSKeyFile,
	}
}

// WriteConfig writes a validated configuration to path in the same form `kenward setup`
// writes it: the same projection, the same section comments, the same 0600.
//
// It exists for the admin dashboard, which edits a household's configuration long after
// setup ran and must not produce a second dialect of the same file. The wizard's own
// finish path and this one go through documentFor and marshalDocument together, so a
// field added to the schema and forgotten in one of them is forgotten in both — which
// is what write_test.go already fails the build over.
//
// force replaces an existing file. The dashboard always passes true: it is editing a
// file it is showing the operator, and refusing would be refusing the whole feature.
// It does not validate: the caller is expected to have run Validate and to have
// something better to do with the problems than a write error.
func WriteConfig(path string, cfg *config.Config, force bool) error {
	data, err := marshalDocument(documentFor(cfg, cfg.DataDir != ""))
	if err != nil {
		return err
	}
	return writeFile(path, data, configFileMode, force)
}

// Describe renders a probe result for an operator: what happened, and — for everything
// but a clean answer — that the endpoint was recorded anyway.
//
// Exported so the dashboard shows the same sentences the terminal wizard does. Somebody
// setting this up on a laptop while the GPU machine is asleep downstairs must not be
// told two different things about the same silence depending on which surface they used.
func (r ProbeResult) Describe() string { return r.describe() }

// LooksLikeBotToken reports whether a string has the shape BotFather hands out.
//
// Exported for the dashboard's own token field, which asks the same question of the
// same value and must give the same answer. It is a question rather than a rule in both
// places — see askToken — so a caller showing "that does not look like one" and then
// accepting it anyway is using this correctly.
func LooksLikeBotToken(s string) bool { return looksLikeBotToken(s) }
