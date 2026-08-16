package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Status is `kenward doctor`, not a second opinion.
//
// doctor already computes every fact this menu wants — whether the configuration
// validates, whether lore answers, which members Telegram authorises a bot for,
// which endpoints are reachable — and it exits 0, 1 or 2 with defined meanings: 0
// success, 1 runtime failure, 2 a configuration or usage error. Reimplementing any of
// it here would produce a tray that disagrees with the command an operator runs when
// the tray worries them, and the tray would be the one that was wrong.
//
// So this runs the real binary with --json and renders what comes back. The struct
// below mirrors only the fields the menu displays; encoding/json ignores the rest, so
// doctor gaining a section does not break this and losing one degrades to a shorter
// menu rather than an error.
type statusReport struct {
	Version string `json:"version"`
	Mode    string `json:"mode"`
	Unit    string `json:"unit,omitempty"`

	Configuration []statusCheck    `json:"configuration"`
	Memory        []statusCheck    `json:"memory"`
	Sessions      []statusCheck    `json:"sessions"`
	Transport     []statusCheck    `json:"transport"`
	Endpoints     []statusEndpoint `json:"endpoints"`

	Exit int `json:"exit_code"`

	// checkedAt and err are this package's own, filled in by runDoctor. They are
	// not part of doctor's JSON.
	checkedAt time.Time
	err       error
}

type statusCheck struct {
	Status string `json:"status"`
	Text   string `json:"text"`
}

type statusEndpoint struct {
	Name    string `json:"name"`
	Reached bool   `json:"reached"`
	Detail  string `json:"detail,omitempty"`
	Millis  int64  `json:"millis,omitempty"`
}

// doctorTimeout bounds one status refresh.
//
// doctor talks to Telegram and to every configured endpoint, and a household endpoint
// that is switched off is the normal case rather than a fault, so this has to be
// longer than a snappy CLI would want. If it is exceeded the menu says so instead of
// showing a stale report as though it were current.
const doctorTimeout = 90 * time.Second

func runDoctor(exe, configPath string) statusReport {
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "doctor", "--config", configPath, "--json")
	out, err := cmd.Output()

	rep := statusReport{checkedAt: time.Now()}
	// A non-zero exit is the normal way doctor reports a finding, and it still
	// writes the whole report to stdout first. Decoding before judging the error is
	// therefore the point: exit 1 with a parseable report is far more useful than
	// "doctor failed".
	if jsonErr := json.Unmarshal(out, &rep); jsonErr != nil {
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			rep.err = fmt.Errorf("doctor exited %d and produced no report", exit.ExitCode())
		case err != nil:
			rep.err = err
		default:
			rep.err = jsonErr
		}
		rep.checkedAt = time.Now()
	}
	return rep
}

// lines renders the report as the Status submenu shows it: doctor's own findings, in
// doctor's own words, with doctor's own symbols. Nothing here rewrites a check's text,
// because a tray that paraphrases is a tray that eventually paraphrases wrongly.
func (r statusReport) lines() []string {
	if r.checkedAt.IsZero() {
		return []string{"Status: checking…"}
	}
	if r.err != nil {
		return []string{
			"Status: could not run `kenward doctor`",
			"  " + r.err.Error(),
		}
	}

	out := []string{fmt.Sprintf("kenward %s — mode: %s", r.Version, r.Mode)}
	if r.Unit != "" {
		out = append(out, "this unit runs only "+r.Unit)
	}
	for _, c := range concat(r.Configuration, r.Memory, r.Sessions, r.Transport) {
		out = append(out, symbol(c.Status)+" "+c.Text)
	}
	for _, ep := range r.Endpoints {
		if ep.Reached {
			out = append(out, fmt.Sprintf("%s %s answered in %dms", symbol("ok"), ep.Name, ep.Millis))
			continue
		}
		// Reported, never failed. A household's GPU box is switched off most of
		// the time and doctor deliberately does not treat that as unhealthy; the
		// icon must not either.
		out = append(out, fmt.Sprintf("%s %s: %s", symbol("warn"), ep.Name, ep.Detail))
	}
	out = append(out, fmt.Sprintf("checked %s · doctor exit %d", r.checkedAt.Format("15:04"), r.Exit))
	return out
}

// settled reports whether doctor has been run and answered. Before that there is no
// verdict, and the icon must not invent one.
func (r statusReport) settled() bool { return !r.checkedAt.IsZero() && r.err == nil }

// healthy reports whether doctor found nothing wrong. Exit 0 is the whole test,
// because doctor's exit codes are a documented contract — 0 success, 1 runtime
// failure, 2 configuration or usage error — and second-guessing them from the check
// list is how the tray comes to disagree with the command.
func (r statusReport) healthy() bool { return r.settled() && r.Exit == 0 }

func symbol(status string) string {
	switch status {
	case "ok":
		return "✓"
	case "warn":
		return "!"
	default:
		return "✗"
	}
}

func concat[T any](groups ...[]T) []T {
	var out []T
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
