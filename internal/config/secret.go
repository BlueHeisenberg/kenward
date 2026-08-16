package config

// Where a secret's value comes from, and how it gets here without being written down.
//
// Configuration names secrets; it never holds one. Until now the only way to supply a
// value was an environment variable, which is right for a shell-launched process and
// wrong in the two deployments that matter most:
//
//   - In a pod, the environment is readable by every process in the container and
//     visible in /proc. A member's pod holds that member's bot token, and whoever holds
//     a bot token reads every message ever sent to it.
//   - Under systemd, the shipped unit supplies secrets as files under
//     $CREDENTIALS_DIRECTORY precisely so they stay out of the environment.
//
// So a secret now has three possible sources, and exactly one of them may be stated:
//
//  1. <name>_file — a path. Trailing line endings are trimmed, because every tool that
//     writes a credential file adds one and a token with "\n" (or "\r\n") on the end
//     fails in a way nobody enjoys diagnosing.
//  2. <name>_env — an environment variable. Unchanged, and still the right answer for a
//     hand-run binary.
//  3. A systemd credential — no configuration at all. If $CREDENTIALS_DIRECTORY is set
//     and holds a credential of the expected name, that is the value. The unit file
//     already names it; making the operator repeat the name in kenward.yaml would only
//     create a second place for the two to disagree.
//
// Stating two sources for one secret is an error rather than a precedence win: two
// sources means someone believes something false about where the value comes from, and
// the belief is worth interrupting. The automatic credential is not a "stated" source —
// it is the fallback when the file says nothing — so it never collides with anything.
//
// Nothing here ever puts a value in a message. Names and paths appear freely; that is
// what makes a fault fixable. Values do not appear in an error, in a log line, or in
// String(), and no resolved value is stored on Config at all — the accessors below hand
// one back on demand, so no amount of %+v on a configuration can print a token.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvCredentialsDirectory is the variable systemd sets for a unit that uses
// LoadCredential=, ImportCredential= or SetCredential=. Its value is a directory holding
// one file per credential, owned by the unit's user and mode 0400, on a tmpfs that never
// reaches disk.
const EnvCredentialsDirectory = "CREDENTIALS_DIRECTORY"

// The credential naming convention. A credentials directory belongs to one unit, so the
// names need no "kenward" prefix; they mirror the configuration field instead, which is
// what an operator reading the unit file next to kenward.yaml needs them to do.
//
//	LoadCredential=bot_token:/etc/kenward/bot-token          # household bot
//	LoadCredential=bot_token.david:/etc/kenward/david-token  # members[].id = david
//	LoadCredential=api_key.openrouter:/etc/kenward/or-key    # endpoints[].name
const (
	// CredentialBotToken supplies telegram.bot_token — the household bot in simple
	// mode.
	CredentialBotToken = "bot_token"
	// credentialBotTokenPrefix is followed by the member's id.
	credentialBotTokenPrefix = "bot_token."
	// credentialAPIKeyPrefix is followed by the endpoint's name.
	credentialAPIKeyPrefix = "api_key."
	// credentialPassphrasePrefix is followed by the member's id. Isolated mode
	// only: it is the passphrase that wraps that one member's session key.
	credentialPassphrasePrefix = "passphrase."
)

// maxSecretFileSize bounds what will be read as a secret. A bot token is under a hundred
// bytes; anything at this size is a path that points somewhere unintended, and reading it
// whole would be the only harm done.
const maxSecretFileSize = 64 << 10

// MemberBotTokenCredential is the systemd credential name for a member's own bot token.
// It returns "" for a member id systemd would not accept as a credential name, which
// disables the automatic lookup for that member rather than guessing at a filename.
func MemberBotTokenCredential(memberID string) string {
	return credentialName(credentialBotTokenPrefix + strings.TrimSpace(memberID))
}

// MemberPassphraseCredential is the systemd credential name for the passphrase that
// wraps one member's session key. Like MemberBotTokenCredential it returns "" for a
// member id systemd would not accept, which disables the automatic lookup for that
// member rather than guessing at a filename.
func MemberPassphraseCredential(memberID string) string {
	return credentialName(credentialPassphrasePrefix + strings.TrimSpace(memberID))
}

// EndpointAPIKeyCredential is the systemd credential name for one endpoint's API key.
// It returns "" for an endpoint name systemd would not accept.
func EndpointAPIKeyCredential(endpoint string) string {
	return credentialName(credentialAPIKeyPrefix + strings.TrimSpace(endpoint))
}

// credentialName returns name if systemd would accept it as a credential name, and ""
// otherwise. systemd allows ASCII letters, digits, underscore, hyphen and dot; the
// filename is the name, so anything else — a slash above all — must not become a path.
func credentialName(name string) string {
	if name == "" || len(name) > 255 || strings.Contains(name, "..") {
		return ""
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return ""
		}
	}
	if strings.HasSuffix(name, ".") {
		return ""
	}
	return name
}

// Secret is a resolved secret value.
//
// The value lives behind a closure rather than in a string field. A string field would
// be printed in full by %#v and by any reflection-based logger, unexported or not; a
// func value prints as an address. Value() is the only way out, and String() says where
// the value came from without saying what it is.
type Secret struct {
	// source describes the origin in the operator's terms: "environment variable
	// KENWARD_BOT_TOKEN", "file /etc/kenward/token", "systemd credential bot_token".
	// It is always safe to print.
	source string
	get    func() string
}

func newSecret(value, source string) Secret {
	return Secret{source: source, get: func() string { return value }}
}

// Value returns the secret. The zero Secret returns "".
func (s Secret) Value() string {
	if s.get == nil {
		return ""
	}
	return s.get()
}

// Source names where the value came from. Safe to log.
func (s Secret) Source() string { return s.source }

// IsSet reports whether a value was resolved. An endpoint that needs no key yields the
// zero Secret, which is not an error.
func (s Secret) IsSet() bool { return s.get != nil }

// String renders the origin, never the value. Both this and GoString exist so that %v,
// %+v and %#v all refuse alike.
func (s Secret) String() string {
	if s.get == nil {
		return "config.Secret(unset)"
	}
	return "config.Secret(from " + s.source + ", value withheld)"
}

// GoString is String: %#v must not be a way around it.
func (s Secret) GoString() string { return s.String() }

// SecretRef is one secret the configuration depends on, described by where its value may
// be found rather than by the value. It holds nothing sensitive and is safe to print.
type SecretRef struct {
	// Where is the configuration path of the secret without a source suffix, such as
	// "telegram.bot_token" or "endpoints[1].api_key". Messages append "_file" or
	// "_env" to it depending on which source is at fault.
	Where string
	// File is the path from <name>_file, empty if unset.
	File string
	// Env is the variable named by <name>_env, empty if unset.
	Env string
	// Credential is the systemd credential consulted when neither is set. Empty
	// disables the automatic lookup.
	Credential string
}

// fields returns the two source field names for this secret, for messages that have to
// tell an operator which lines to look at.
func (r SecretRef) fields() (fileField, envField string) {
	name := r.Where
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name + "_file", name + "_env"
}

// stated reports whether the operator wrote a source down for this secret.
func (r SecretRef) stated() bool { return r.File != "" || r.Env != "" }

// bothSourcesDetail is the sentence for a secret that names two sources. Resolve says it
// for the secrets a mode uses and validateSecretSources for the rest, and they say it in
// the same words because it is the same fault.
func bothSourcesDetail(fileField, envField string) string {
	return fmt.Sprintf("%s and %s are both set; a secret has exactly one source, and naming two means one of them is a belief about where the value comes from that is not true — remove one",
		fileField, envField)
}

// SecretError explains why a secret could not be resolved. It names the field, the
// variable, the path and the mode; it never carries a value.
type SecretError struct {
	// Where is the configuration path the operator should look at, including the
	// source suffix when one source is at fault.
	Where string
	// Detail is the fault in the operator's terms.
	Detail string
	// MissingEnv is the variable name when the fault is an unset or empty environment
	// variable, so validation can still print an export list.
	MissingEnv string
	// NotFound means no source supplied a value at all: nothing was stated and no
	// credential was there. Whether that is a fault depends on the caller — an
	// endpoint on the household's own network needs no key.
	NotFound bool
}

func (e *SecretError) Error() string { return e.Where + ": " + e.Detail }

// SecretFS reads a credential file and reports the mode of the file it actually opened.
//
// Read and stat are one call on purpose: the mode check has to apply to the file whose
// bytes were read, not to whatever the path resolved to a moment earlier. It is an
// interface so that tests exercise permissive modes and missing files without a real
// filesystem, which on a developer's machine cannot be made to hold a 0644 secret
// portably anyway.
type SecretFS interface {
	ReadSecretFile(path string) (data []byte, mode fs.FileMode, err error)
}

// OSSecretFS reads the real filesystem. It is the default.
type OSSecretFS struct{}

// ReadSecretFile opens the path, stats the open file, and reads at most
// maxSecretFileSize bytes.
func (OSSecretFS) ReadSecretFile(path string) ([]byte, fs.FileMode, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	mode := info.Mode()
	data, err := io.ReadAll(io.LimitReader(f, maxSecretFileSize+1))
	if err != nil {
		return nil, mode, err
	}
	if len(data) > maxSecretFileSize {
		return nil, mode, fmt.Errorf("larger than %d bytes; that is not a credential", maxSecretFileSize)
	}
	return data, mode, nil
}

// FileModePolicy selects whether a secret file's permission bits are checked.
type FileModePolicy int

const (
	// FileModeDefault checks on Unix and skips on Windows. See checkMode for why.
	FileModeDefault FileModePolicy = iota
	// FileModeEnforce checks everywhere. Tests use it; so may an operator who knows
	// their Windows filesystem reports meaningful bits.
	FileModeEnforce
	// FileModeSkip never checks.
	FileModeSkip
)

// SecretOptions configures a Secrets resolver. Every field is a seam a test can hold
// still: no test of this package may read the real environment or a real credentials
// directory, both of which are global state shared with every other test in the binary.
type SecretOptions struct {
	// LookupEnv reads the environment. Nil means the process environment.
	LookupEnv LookupEnvFunc
	// FS reads secret files. Nil means the real filesystem.
	FS SecretFS
	// CredentialsDir overrides the credentials directory. Empty means whatever
	// LookupEnv reports for CREDENTIALS_DIRECTORY, which is usually nothing.
	CredentialsDir string
	// FileMode selects the permission check.
	FileMode FileModePolicy
}

// Secrets resolves SecretRefs against an environment, a filesystem and a credentials
// directory. It caches nothing: a secret is read at the moment it is asked for, so a
// rotated credential file is picked up without a restart, and a value read during
// validation is dropped as soon as it has been proved readable.
type Secrets struct {
	lookupEnv LookupEnvFunc
	fsys      SecretFS
	credsDir  string
	fileMode  FileModePolicy
}

// NewSecrets builds a resolver. The zero SecretOptions gives the process environment,
// the real filesystem and the platform's default mode policy.
func NewSecrets(opts SecretOptions) *Secrets {
	s := &Secrets{
		lookupEnv: opts.LookupEnv,
		fsys:      opts.FS,
		credsDir:  opts.CredentialsDir,
		fileMode:  opts.FileMode,
	}
	if s.lookupEnv == nil {
		s.lookupEnv = os.LookupEnv
	}
	if s.fsys == nil {
		s.fsys = OSSecretFS{}
	}
	if s.credsDir == "" {
		if dir, ok := s.lookupEnv(EnvCredentialsDirectory); ok {
			s.credsDir = strings.TrimSpace(dir)
		}
	}
	return s
}

// orDefault makes a nil *Secrets behave like one built from the process environment, the
// way a nil LookupEnvFunc already does.
func (s *Secrets) orDefault() *Secrets {
	if s == nil {
		return NewSecrets(SecretOptions{})
	}
	return s
}

// Resolve reads the value for one reference.
//
// It returns a *SecretError for every fault, with NotFound set when nothing supplied a
// value; a caller for whom that is acceptable — an endpoint that needs no key — checks
// for it rather than treating absence as failure.
func (s *Secrets) Resolve(ref SecretRef) (Secret, error) {
	s = s.orDefault()
	fileField, envField := ref.fields()

	switch {
	case ref.File != "" && ref.Env != "":
		return Secret{}, &SecretError{Where: ref.Where, Detail: bothSourcesDetail(fileField, envField)}

	case ref.File != "":
		v, err := s.readSecretFile(ref.File)
		if err != nil {
			return Secret{}, &SecretError{Where: ref.Where + "_file", Detail: err.Error()}
		}
		return newSecret(v, "file "+ref.File), nil

	case ref.Env != "":
		v, ok := s.lookupEnv(ref.Env)
		switch {
		case !ok:
			return Secret{}, &SecretError{
				Where:      ref.Where + "_env",
				Detail:     fmt.Sprintf("environment variable %s is not set", ref.Env),
				MissingEnv: ref.Env,
			}
		case strings.TrimSpace(v) == "":
			return Secret{}, &SecretError{
				Where:      ref.Where + "_env",
				Detail:     fmt.Sprintf("environment variable %s is set but empty", ref.Env),
				MissingEnv: ref.Env,
			}
		}
		return newSecret(v, "environment variable "+ref.Env), nil
	}

	return s.resolveCredential(ref, fileField, envField)
}

// resolveCredential handles the unstated case: a systemd credential if one is there,
// NotFound if not.
func (s *Secrets) resolveCredential(ref SecretRef, fileField, envField string) (Secret, error) {
	notFound := func() (Secret, error) {
		detail := fmt.Sprintf("no source; set %s or %s", fileField, envField)
		if ref.Credential != "" {
			detail += fmt.Sprintf(", or supply the systemd credential %q", ref.Credential)
		}
		return Secret{}, &SecretError{Where: ref.Where, Detail: detail, NotFound: true}
	}
	if s.credsDir == "" || ref.Credential == "" {
		return notFound()
	}

	path := filepath.Join(s.credsDir, ref.Credential)
	v, err := s.readSecretFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The unit does not carry this credential. That is the ordinary case for
		// every deployment that supplies secrets another way.
		return notFound()
	case err != nil:
		return Secret{}, &SecretError{
			Where:  ref.Where,
			Detail: fmt.Sprintf("systemd credential %s: %v", ref.Credential, err),
		}
	}
	return newSecret(v, "systemd credential "+ref.Credential), nil
}

// readSecretFile reads a credential file, refuses a permissive one, and trims trailing
// line endings. The returned error names the path and the mode and nothing else.
func (s *Secrets) readSecretFile(path string) (string, error) {
	data, mode, err := s.fsys.ReadSecretFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Wrapped, not flattened: the caller looking for a systemd credential
			// has to be able to tell "this unit does not carry one" from "this
			// unit carries one that cannot be read".
			return "", fmt.Errorf("%s: %w", path, fs.ErrNotExist)
		}
		return "", fmt.Errorf("%s: %v", path, err)
	}
	if err := s.checkMode(path, mode); err != nil {
		return "", err
	}
	// Trim trailing line endings, and only those: printf, editors, `echo`, Kubernetes
	// and systemd's own credential tooling all add one, an editor configured for CRLF
	// adds two bytes, and a token carrying either is rejected by Telegram with an error
	// that names nothing useful. Interior whitespace is left alone — a secret with a
	// space in the middle is a secret, not a mistake — and a file that is nothing but
	// newlines is reported empty rather than resolved to "".
	v := strings.TrimRight(string(data), "\r\n")
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%s: file is empty", path)
	}
	return v, nil
}

// checkMode refuses a secret file that anyone but its owner can read.
//
// A 0644 token file is a finding, not a preference: everything with a shell on that host
// holds the household's bot. Failing loudly, with the mode in the message, is the only
// way the operator who created it ever learns.
//
// The check is skipped on Windows. os.Stat there synthesises permission bits from the
// read-only attribute alone — every readable file reports 0666, every read-only one 0444
// — and none of it reflects the ACL that actually governs access. Enforcing would refuse
// every file on the platform while proving nothing about any of them; the honest answer
// is to say so rather than to imply a check that is not happening.
func (s *Secrets) checkMode(path string, mode fs.FileMode) error {
	switch s.fileMode {
	case FileModeSkip:
		return nil
	case FileModeDefault:
		if runtime.GOOS == "windows" {
			return nil
		}
	}
	if perm := mode.Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s: mode %04o is readable by group or others; a secret file must be %04o (chmod 600 %s)",
			path, perm, 0o600, path)
	}
	return nil
}

// --- accessors -------------------------------------------------------------------
//
// These are how a secret leaves this package. Each returns the value on demand rather
// than storing it on the Config, so there is no field for a formatting verb to reach.

// BotTokenRef describes the household bot's token: simple mode's one bot.
func (c *Config) BotTokenRef() SecretRef {
	return SecretRef{
		Where:      "telegram.bot_token",
		File:       strings.TrimSpace(c.Telegram.BotTokenFile),
		Env:        strings.TrimSpace(c.Telegram.BotTokenEnv),
		Credential: CredentialBotToken,
	}
}

// BotToken resolves the household bot's token. It is an error for it to be absent: a
// household in simple mode without its bot serves nobody.
func (c *Config) BotToken(s *Secrets) (Secret, error) { return s.orDefault().Resolve(c.BotTokenRef()) }

// BotTokenRef describes this member's own bot token, used in isolated mode where the
// member's pod carries it alone. The reference names the member by id rather than by
// index, because a member outlives their position in the file.
func (m MemberConfig) BotTokenRef() SecretRef {
	return SecretRef{
		Where:      fmt.Sprintf("members[%s].bot_token", m.ID),
		File:       strings.TrimSpace(m.BotTokenFile),
		Env:        strings.TrimSpace(m.BotTokenEnv),
		Credential: MemberBotTokenCredential(m.ID),
	}
}

// BotToken resolves this member's own bot token.
func (m MemberConfig) BotToken(s *Secrets) (Secret, error) {
	return s.orDefault().Resolve(m.BotTokenRef())
}

// PassphraseRef describes the passphrase that wraps this member's session key: what
// isolated mode needs and simple mode has no use for, where one node passphrase wraps
// everybody's.
//
// Naming it here rather than deriving a variable name from the member id is the same
// choice bot tokens made, for the same reason: an id is not an environment variable
// name, and any rule turning one into the other maps distinct ids onto one name — which
// for a passphrase means two members silently sharing a wrapping secret.
func (m MemberConfig) PassphraseRef() SecretRef {
	return SecretRef{
		Where:      fmt.Sprintf("members[%s].passphrase", m.ID),
		File:       strings.TrimSpace(m.PassphraseFile),
		Env:        strings.TrimSpace(m.PassphraseEnv),
		Credential: MemberPassphraseCredential(m.ID),
	}
}

// Passphrase resolves the passphrase that wraps this member's session key.
func (m MemberConfig) Passphrase(s *Secrets) (Secret, error) {
	return s.orDefault().Resolve(m.PassphraseRef())
}

// SessionPassphraseRef describes simple mode's one node passphrase, which wraps every
// member's session key.
//
// It exists because its absence was the one secret nothing checked. A member's
// passphrase is a configuration field, so an isolated pod handed no passphrase is
// refused at load with the variable named; simple mode's had no field, so the same
// mistake — the one deploy/compose.simple.yml shipped with — got as far as the session
// manager and restart-looped there instead.
//
// Credential is deliberately empty. The systemd credential for a node passphrase is
// read by cmd/kenward's readPassphrase directly, under a name that predates this
// reference, and two code paths reading one file under two sets of rules is worse than
// one path fewer here: this reference covers the two sources the file can state, and
// resolves to NotFound when it states neither, which is exactly the case where the
// other three mechanisms are meant to apply.
func (c *Config) SessionPassphraseRef() SecretRef {
	return SecretRef{
		Where: "session.passphrase",
		File:  strings.TrimSpace(c.Session.PassphraseFile),
		Env:   strings.TrimSpace(c.Session.PassphraseEnv),
	}
}

// APIKeyRef describes this endpoint's key.
func (e EndpointConfig) APIKeyRef() SecretRef {
	return SecretRef{
		Where:      fmt.Sprintf("endpoints[%s].api_key", e.Name),
		File:       strings.TrimSpace(e.APIKeyFile),
		Env:        strings.TrimSpace(e.APIKeyEnv),
		Credential: EndpointAPIKeyCredential(e.Name),
	}
}

// APIKey resolves this endpoint's key.
//
// An endpoint may legitimately need none — the usual case for a machine on the
// household's own network — so absence is reported as the zero Secret and a nil error,
// and only a stated source that cannot be read is a failure.
func (e EndpointConfig) APIKey(s *Secrets) (Secret, error) {
	sec, err := s.orDefault().Resolve(e.APIKeyRef())
	var se *SecretError
	if errors.As(err, &se) && se.NotFound {
		return Secret{}, nil
	}
	return sec, err
}
