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

// TestDocumentCoversTheWholeSchema is the guard on the one shortcut this package
// takes: it writes YAML through a mirror of config.Config rather than through
// config.Config itself, so that generated files can leave empty fields out.
//
// A field added to the schema and not added here would simply never be written, and
// nothing else would notice — the file would still parse, still validate, and
// quietly lack whatever it was. So the two shapes are compared by reflection.
func TestDocumentCoversTheWholeSchema(t *testing.T) {
	want := yamlKeys(reflect.TypeOf(config.Config{}), "")
	got := yamlKeys(reflect.TypeOf(document{}), "")

	if !reflect.DeepEqual(want, got) {
		t.Errorf("the written document no longer matches config.Config.\n"+
			"in the schema but not written: %v\n"+
			"written but not in the schema:  %v",
			missing(want, got), missing(got, want))
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
		"lore_command: [lore, mcp]", // and so does an argv
		"idle_timeout: 30m0s",       // defaults are visible rather than implied
		"max_proposals_per_turn: 1", //
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
