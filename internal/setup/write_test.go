package setup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// notWritten lists the schema fields the wizard deliberately never emits, each with
// the reason. Anything not on this list that the wizard fails to write is a field
// that will silently never appear in a generated configuration.
//
// The first four entries are the *_file half of a secret source. The wizard writes the
// *_env half, for two reasons:
//
//   - It is the one form that works in every deployment. The file form needs a path
//     that already exists with mode 0600 — which the wizard would have to ask for,
//     and could not sensibly create on the operator's behalf — and the credential
//     form needs systemd.
//   - Choosing between three secret-delivery mechanisms is not a question a
//     household can answer. The file and credential forms exist for operators who
//     have chosen a deployment that offers something better — a systemd unit, a
//     mounted secret — and those people are editing this file by hand anyway.
//
// All three forms are read by the runtime, so this is a choice about what to ask a
// person standing at a kitchen table, not a limit on what kenward supports.
//
// Stating both forms for one secret is a validation error rather than a precedence
// (see config/secret.go), so emitting the file form "as well" is not an option
// either: it would be the one configuration the loader refuses.
//
// session.passphrase_* is a fifth entry and a different case: not a choice between two
// forms of one source, but a choice the wizard is in no position to make. Simple mode's
// node passphrase has four legitimate deliveries — a named variable, a named file, a
// systemd credential, and somebody typing it — and only the first two go in the file. A
// named source that the deployment does not supply is a validation error, so a wizard
// that wrote session.passphrase_env: KENWARD_PASSPHRASE would refuse to load under the
// systemd unit this project ships, which delivers exactly that secret by
// LoadCredential=kenward-passphrase. Writing nothing keeps all four open; the refusal at
// startup names all four, and an operator who has chosen one adds the line themselves.
var notWritten = map[string]string{
	"telegram.bot_token_file": "the wizard writes bot_token_env; naming both sources is a validation error",
	"members.bot_token_file":  "the wizard writes bot_token_env; naming both sources is a validation error",
	"members.passphrase_file": "the wizard writes passphrase_env; naming both sources is a validation error",
	"endpoints.api_key_file":  "the wizard writes api_key_env; naming both sources is a validation error",
	"session.passphrase_env":  "the wizard cannot know which of the four node-passphrase deliveries this household uses, and naming the wrong one is a refusal to load",
	"session.passphrase_file": "as session.passphrase_env; the wizard names no source for the node passphrase",
	// The whole reminders section. Every key in it has a default that is right for a
	// household that has never thought about reminders, and the one question a wizard
	// could usefully ask — how many unprompted messages a day are welcome — is one
	// nobody can answer before they have lived with the thing for a week. A generated
	// configuration that stated the defaults would only be four more lines to read at
	// the kitchen table on the day they are least useful.
	"reminders":                 "the section as a whole; every key in it is defaulted, and the reasons are below",
	"reminders.timezone":        "empty means the machine's own clock, which is right for a node sitting in the house it serves",
	"reminders.max_per_day":     "the default is right until a household has lived with reminders long enough to disagree",
	"reminders.catch_up_window": "as reminders.max_per_day",
	"reminders.max_stored":      "as reminders.max_per_day",
}

// TestDocumentCoversTheWholeSchema is the guard on the one shortcut this package
// takes: it writes YAML through a mirror of config.Config rather than through
// config.Config itself, so that generated files can leave empty fields out.
//
// A field added to the schema and not added here would simply never be written, and
// nothing else would notice — the file would still parse, still validate, and
// quietly lack whatever it was. So the two shapes are compared by reflection, and
// the only permitted differences are the ones named in notWritten with a reason.
func TestDocumentCoversTheWholeSchema(t *testing.T) {
	schema := yamlKeys(reflect.TypeOf(config.Config{}), "")
	written := yamlKeys(reflect.TypeOf(document{}), "")

	var want []string
	for _, key := range schema {
		if _, skip := notWritten[key]; !skip {
			want = append(want, key)
		}
	}

	if !reflect.DeepEqual(want, written) {
		t.Errorf("the written document no longer matches config.Config.\n"+
			"in the schema but not written: %v\n"+
			"written but not in the schema:  %v\n"+
			"either add the field to document in write.go, or add it to notWritten "+
			"with a comment explaining why a generated configuration is right without it",
			missing(want, written), missing(written, want))
	}
}

// TestNotWrittenNamesRealFields guards the allow-list itself, the way
// internal/config guards its own: an entry that no longer names a schema field is an
// exception being granted to nothing, and it would go on excusing whatever field
// happened to inherit the name.
func TestNotWrittenNamesRealFields(t *testing.T) {
	schema := make(map[string]bool)
	for _, key := range yamlKeys(reflect.TypeOf(config.Config{}), "") {
		schema[key] = true
	}
	for key := range notWritten {
		if !schema[key] {
			t.Errorf("notWritten names %q, which is not a field of config.Config any more; remove it or fix the path", key)
		}
	}
}

// TestTheWizardWritesTheEnvFormOfEverySecret states the other half of the decision
// above as an assertion rather than a comment: every secret is named by environment
// variable, and none by file, in both modes and for members whose token does not
// exist yet. The runtime reads all three forms, so what this pins is the wizard's
// answer to a question it declines to ask.
func TestTheWizardWritesTheEnvFormOfEverySecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	answers := []string{
		"2", "Home", "1", realToken, "n",
		"David", "", "1",
		"monster", "http://monster.tail:8000/v1", "q", "n", "local", "y",
		"openrouter", "https://openrouter.ai/api/v1", "sonnet", "y", "OPENROUTER_API_KEY", "sk-x", "cloud", "n",
		"n", "n",
		"", // conversation reset: off
	}
	if _, _, io, err := runWizard(t, "linux", Options{ConfigPath: path}, answers...); err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{"bot_token_env:", "bot_token_env: KENWARD_BOT_TOKEN_DAVID", "api_key_env:"} {
		if !strings.Contains(body, want) {
			t.Errorf("the written file does not contain %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"bot_token_file", "api_key_file"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the wizard wrote %q; it has no path to write there and no question that would get it one", unwanted)
		}
	}
}

// yamlKeys walks a struct and returns every yaml key path in it, sorted. Fields
// tagged "-" are skipped: config.MemberConfig.EnrolledAt is deliberately not
// readable from the file.
func yamlKeys(t reflect.Type, prefix string) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || name == "" {
			continue
		}
		path := prefix + name
		keys = append(keys, path)

		ft := f.Type
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		// A block the generated file omits entirely is written as a pointer to its
		// own doc type, so that yaml.v3's omitempty actually drops it. Without this
		// deref the guard would see the block's own key and none of its fields,
		// which is the exact silent gap it exists to catch.
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			keys = append(keys, yamlKeys(ft, path+".")...)
		}
	}
	sort.Strings(keys)
	return keys
}

func missing(from, in []string) []string {
	have := make(map[string]bool, len(in))
	for _, k := range in {
		have[k] = true
	}
	var out []string
	for _, k := range from {
		if !have[k] {
			out = append(out, k)
		}
	}
	return out
}

// TestWrittenFileDecodesBackToWhatWasBuilt closes the loop on the mirror: the
// configuration the wizard returned and the configuration its file decodes to are
// the same object.
func TestWrittenFileDecodesBackToWhatWasBuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	_, built, io, err := runWizard(t, "linux", Options{ConfigPath: path}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoded, err := config.Decode(f)
	if err != nil {
		t.Fatalf("decoding what was written: %v", err)
	}
	if !reflect.DeepEqual(built, decoded) {
		t.Errorf("the file does not decode back to the configuration that was built.\nbuilt:   %+v\ndecoded: %+v", built, decoded)
	}
}

// TestTheFileReadsLikeSomebodyWroteIt is a soft assertion about a hard requirement:
// this file is hand-edited, and a person who opens it and finds machine output is a
// person who will not edit it.
func TestTheFileReadsLikeSomebodyWroteIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultConfigFileName)
	if _, _, io, err := runWizard(t, "linux", Options{ConfigPath: path}, simpleAnswers()...); err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)

	for _, want := range []string{
		"# kenward.yaml",
		"# The household itself",
		"tiers: [local]",            // a chain reads as a chain, not as a bullet list
		"lore_command: [lore]",      // and so does an argv
		"idle_timeout: 0s",          // defaults are visible rather than implied, including "off"
		"max_proposals_per_turn: 1", //
		// The wizard cannot learn an endpoint's window or its completion cap — it
		// probes that an address answers, not what the server behind it was started
		// with — so it writes the defaults out where the operator can correct them.
		// A machine bought for the size of its window is wasted silently otherwise.
		"context_window: 16384",
		"max_completion_tokens: 4096",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the written file does not contain %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{
		"group_chat_id: 0", // no group is configured yet; saying "0" invites editing it to something wrong
		"telegram_id: 0",   // nobody has enrolled yet
		`api_key_env: ""`,  // the local machine needs no key
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the written file contains the noise field %q", unwanted)
		}
	}
}

func TestWriteFileRefusesToClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thing.yaml")
	if err := writeFile(path, []byte("first\n"), 0o600, false); err != nil {
		t.Fatal(err)
	}
	err := writeFile(path, []byte("second\n"), 0o600, false)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the file: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\n" {
		t.Error("the file was modified anyway")
	}
}

func TestWriteFileCreatesMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "thing.yaml")
	if err := writeFile(path, []byte("x\n"), 0o600, false); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits to assert on; the .env test covers what can be checked there")
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := writeFile(path, []byte("x\n"), 0o600, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %O, want 0600", perm)
	}
}

func TestRenderEnvFileSkipsValuesItDoesNotHave(t *testing.T) {
	got := string(renderEnvFile([]EnvVar{
		{Name: "KENWARD_BOT_TOKEN", value: "123:abc"},
		{Name: "OPENROUTER_API_KEY"}, // no value: the operator will set it themselves
	}))
	if !strings.Contains(got, "KENWARD_BOT_TOKEN=123:abc") {
		t.Errorf("missing the variable that has a value:\n%s", got)
	}
	// Writing OPENROUTER_API_KEY= would set it to nothing, which config.Validate
	// rejects for exactly the same reason kenward would fail on it.
	if strings.Contains(got, "OPENROUTER_API_KEY") {
		t.Errorf("a variable with no value was written:\n%s", got)
	}
}

func TestQuoteEnvValue(t *testing.T) {
	for in, want := range map[string]string{
		"123456:AA-bb_cc":  "123456:AA-bb_cc",
		"sk-or-v1-abc.def": "sk-or-v1-abc.def",
		"":                 "''",
		"with space":       "'with space'",
		"with#hash":        "'with#hash'",
		`it's`:             `'it'\''s'`,
		"line\nbreak":      "'line\nbreak'",
	} {
		if got := quoteEnvValue(in); got != want {
			t.Errorf("quoteEnvValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMarshalDocumentIsValidYAML(t *testing.T) {
	doc := documentFor(&config.Config{
		Mode:      config.ModeSimple,
		Household: config.HouseholdConfig{Name: "Home", SharedSpace: "household", Tiers: []string{"local"}},
		Telegram:  config.TelegramConfig{BotTokenEnv: "KENWARD_BOT_TOKEN"},
		Members: []config.MemberConfig{
			{ID: "david", Name: "David", PrivateSpace: "david-private", Tiers: []string{"local"}},
		},
		Endpoints: []config.EndpointConfig{
			{Name: "monster", BaseURL: "http://monster.tail:8000/v1", Model: "q", Tags: []string{"local"}},
		},
		Memory: config.MemoryConfig{LoreCommand: DefaultLoreCommand, SearchLimit: 8},
	}, false)

	data, err := marshalDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("the marshalled document does not decode: %v\n%s", err, data)
	}
}
