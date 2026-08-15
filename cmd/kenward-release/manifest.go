package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BlueHeisenberg/keel/update"
)

// defaultPlatforms is the set of targets a release is expected to carry. It
// mirrors CROSS_TARGETS in Taskfile.yml. A manifest published without one of
// these silently strands every installation on that platform — it will fetch
// the manifest, find no artifact for itself, and simply never update — so a
// missing platform is an error here rather than a note in the output.
var defaultPlatforms = []string{
	"linux/amd64",
	"linux/arm64",
	"darwin/amd64",
	"darwin/arm64",
	"windows/amd64",
}

// defaultBaseURL is where the artifacts named by a manifest are expected to
// be published. {version} and {file} are substituted per artifact.
const defaultBaseURL = "https://github.com/BlueHeisenberg/kenward/releases/download/{version}/{file}"

const manifestUsage = `kenward-release manifest --version V --channel C --dist DIR --notes TEXT --out FILE

Walks DIR, works out which platform each build is for from its filename,
computes each one's SHA-256 and size, and writes an unsigned manifest. The
manifest is what "kenward-release sign" then signs; publishing an unsigned one
is useless, because kenward refuses anything unsigned.

It fails if any expected platform is missing from DIR, because a manifest that
omits a platform strands every installation on it, quietly and forever.

Flags:
  --version V            Release version, e.g. v0.2.0.
  --channel C            Channel(s) this release goes to: stable, edge, or
                         "edge,stable" to publish once to both. The stable
                         delay is enforced by the client against the
                         publication timestamp, so publishing to both at once
                         is the intended thing to do.
  --dist DIR             Directory of cross-compiled builds ("task cross").
  --notes TEXT           Release notes. Say what changes for a household.
  --out FILE             Where to write the manifest; "-" for stdout.
  --security-sensitive   Set if this release changes routing behaviour, tier
                         defaults, key handling, the enrolment path, or what
                         any privacy statement claims. Such a release is never
                         applied without asking the household first. If you
                         are unsure whether it qualifies, it qualifies.
  --base-url URL         Template for artifact URLs; {version} and {file} are
                         substituted. Default:
                         ` + defaultBaseURL + `
  --platforms LIST       Comma-separated platforms that must be present.
                         Default: ` + "linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64" + `
  --published-at TIME    RFC3339 publication timestamp. Defaults to now.
`

func cmdManifest(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	version := fs.String("version", "", "release version, e.g. v0.2.0")
	channels := fs.String("channel", "", "channel(s) to publish to: stable, edge, or edge,stable")
	dist := fs.String("dist", "", "directory of cross-compiled builds")
	notes := fs.String("notes", "", "release notes")
	out := fs.String("out", "", `where to write the manifest, or "-" for stdout`)
	sensitive := fs.Bool("security-sensitive", false, "this release changes security-relevant behaviour")
	baseURL := fs.String("base-url", defaultBaseURL, "artifact URL template")
	platforms := fs.String("platforms", strings.Join(defaultPlatforms, ","), "platforms that must be present")
	publishedAt := fs.String("published-at", "", "RFC3339 publication timestamp (default: now)")
	if err := parseFlags(fs, manifestUsage, args, stdout, stderr); err != nil {
		return err
	}
	for _, required := range []struct{ name, value string }{
		{"version", *version}, {"channel", *channels}, {"dist", *dist}, {"out", *out},
	} {
		if err := requireFlag(required.name, required.value); err != nil {
			return err
		}
	}

	// Parsed, not merely trusted: keel refuses a manifest whose version it
	// cannot parse, and finding that out after publishing is no good.
	if _, err := update.ParseVersion(*version); err != nil {
		return &usageError{err: err}
	}
	chans, err := parseChannels(*channels)
	if err != nil {
		return err
	}
	want, err := parsePlatformList(*platforms)
	if err != nil {
		return err
	}
	published := time.Now().UTC().Truncate(time.Second)
	if *publishedAt != "" {
		published, err = time.Parse(time.RFC3339, *publishedAt)
		if err != nil {
			return usagef("--published-at %q is not an RFC3339 timestamp: %v", *publishedAt, err)
		}
		published = published.UTC()
	}
	if strings.TrimSpace(*notes) == "" {
		fmt.Fprint(stderr, "warning: --notes is empty. A household reads these; say what changes for them.\n")
	}

	artifacts, err := scanDist(*dist, *version, *baseURL)
	if err != nil {
		return err
	}
	if missing := missingPlatforms(want, artifacts); len(missing) > 0 {
		return fmt.Errorf("no build in %s for %s — every installation on %s would be stranded; "+
			"run \"task cross\" and try again",
			*dist, strings.Join(missing, ", "), pluralPlatform(missing))
	}

	release := update.Release{
		Version:           *version,
		Notes:             *notes,
		PublishedAt:       published,
		SecuritySensitive: *sensitive,
		Artifacts:         artifacts,
	}
	m := update.Manifest{
		Schema:      update.ManifestSchema,
		GeneratedAt: published,
		Channels:    map[string]update.Release{},
	}
	for _, c := range chans {
		m.Channels[c] = release
	}

	// Indented, because a human reads this file between building it and
	// signing it. sign re-marshals the payload compactly before signing, so
	// the whitespace here never reaches a signature.
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	data = append(data, '\n')
	if err := writeOut(*out, data, stdout); err != nil {
		return err
	}

	fmt.Fprintf(stderr, "%s → %s (%d artifacts)\n", *version, strings.Join(chans, ", "), len(artifacts))
	if *sensitive {
		fmt.Fprint(stderr, "securitySensitive is set: this release will not auto-apply anywhere.\n")
	}
	return nil
}

// scanDist hashes every recognisable build in dir and turns it into the
// artifact map keel expects, keyed "GOOS/GOARCH".
func scanDist(dir, version, baseURL string) (map[string]update.Artifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read --dist: %w", err)
	}
	artifacts := map[string]update.Artifact{}
	seen := map[string]string{} // platform -> filename, to catch two builds claiming one platform
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		platform, ok := platformFromFilename(e.Name())
		if !ok {
			continue // checksums, notes, whatever else the operator left here
		}
		if prev, dup := seen[platform]; dup {
			return nil, fmt.Errorf("both %s and %s claim to be the %s build; "+
				"clear --dist and rebuild rather than guessing", prev, e.Name(), platform)
		}
		path := filepath.Join(dir, e.Name())
		sum, size, err := hashFile(path)
		if err != nil {
			return nil, err
		}
		seen[platform] = e.Name()
		artifacts[platform] = update.Artifact{
			URL:    artifactURL(baseURL, version, e.Name()),
			SHA256: sum,
			Size:   size,
		}
	}
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("no builds found in %s — file names must look like kenward_<goos>_<goarch>", dir)
	}
	return artifacts, nil
}

func hashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", path, err)
	}
	// Lowercase hex: keel lowercases the manifest's digest before
	// comparing, but a manifest is read by people too.
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func artifactURL(baseURL, version, file string) string {
	if strings.Contains(baseURL, "{version}") || strings.Contains(baseURL, "{file}") {
		r := strings.NewReplacer("{version}", version, "{file}", file)
		return r.Replace(baseURL)
	}
	return strings.TrimSuffix(baseURL, "/") + "/" + file
}

// knownGOOS and knownGOARCH bound what a filename is allowed to claim. A
// typo'd filename becomes an unrecognised file rather than a platform key no
// installation will ever match.
var (
	knownGOOS = map[string]bool{
		"linux": true, "darwin": true, "windows": true,
		"freebsd": true, "openbsd": true, "netbsd": true,
	}
	knownGOARCH = map[string]bool{
		"amd64": true, "arm64": true, "arm": true, "386": true,
		"riscv64": true, "ppc64le": true, "s390x": true, "loong64": true,
	}
)

// platformFromFilename reads "GOOS/GOARCH" out of a build's filename, in the
// shape Taskfile.yml's cross task produces: kenward_<goos>_<goarch>, plus
// ".exe" on Windows. The leading name is not pinned to "kenward" so renaming
// the binary does not silently drop every artifact from the manifest.
func platformFromFilename(name string) (string, bool) {
	base := name
	isExe := strings.HasSuffix(strings.ToLower(base), ".exe")
	if isExe {
		base = base[:len(base)-len(".exe")]
	}
	// Any other extension survives into the last field and fails the GOARCH
	// check there: "kenward_linux_amd64.sha256" ends in "amd64.sha256",
	// which is not an architecture, so checksum and note files alongside the
	// builds are ignored rather than published as artifacts.
	parts := strings.Split(base, "_")
	if len(parts) < 3 {
		return "", false
	}
	goos, goarch := parts[len(parts)-2], parts[len(parts)-1]
	if !knownGOOS[goos] || !knownGOARCH[goarch] {
		return "", false
	}
	if (goos == "windows") != isExe {
		return "", false // a windows build without .exe, or a unix one with it
	}
	return goos + "/" + goarch, true
}

func parseChannels(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		c := strings.TrimSpace(part)
		if c == "" {
			continue
		}
		switch update.Channel(c) {
		case update.ChannelStable, update.ChannelEdge:
		case update.ChannelOff:
			return nil, usagef(`channel "off" means "never update"; it is not something a manifest can carry`)
		default:
			return nil, usagef("unknown channel %q (want stable or edge)", c)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, usagef("--channel names no channel")
	}
	return out, nil
}

func parsePlatformList(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		goos, goarch, ok := strings.Cut(p, "/")
		if !ok || !knownGOOS[goos] || !knownGOARCH[goarch] {
			return nil, usagef("--platforms: %q is not a GOOS/GOARCH pair I recognise", p)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, usagef("--platforms is empty")
	}
	return out, nil
}

func missingPlatforms(want []string, have map[string]update.Artifact) []string {
	var missing []string
	for _, p := range want {
		if _, ok := have[p]; !ok {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	return missing
}

// writeOut writes data to path, or to stdout when path is "-". A manifest is
// regenerable, so unlike a key file it may be overwritten.
func writeOut(path string, data []byte, stdout io.Writer) error {
	if path == "-" {
		_, err := stdout.Write(data)
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func pluralPlatform(missing []string) string {
	return plural(len(missing), "that platform", "those platforms")
}
