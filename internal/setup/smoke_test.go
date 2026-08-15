package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSmokeDump(t *testing.T) {
	if os.Getenv("SETUP_SMOKE") == "" {
		t.Skip("set SETUP_SMOKE=1 to dump a transcript")
	}
	dir := t.TempDir()
	io := NewScriptIO(
		"1",
		"Casa", "household",
		"123456789:AAF-3jkkQ_pP8vd2X9j1kZq7wRsTuVwXyZ0", "y",
		"David", "María", "",
		"monster", "http://monster.tail:8000/v1", "qwen3.6-27b-awq", "n", "local", "y",
		"openrouter", "https://openrouter.ai/api/v1", "anthropic/claude-sonnet-5", "y", "OPENROUTER_API_KEY", "sk-test", "cloud", "n",
		"n", "n", "n",
	)
	w := New(io, Options{
		ConfigPath: filepath.Join(dir, "kenward.yaml"),
		GOOS:       "linux",
		Probe:      fixedProbe(Answered),
		LookupEnv:  func(string) (string, bool) { return "", false },
	})
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	t.Log("\n" + io.Transcript())
	data, _ := os.ReadFile(filepath.Join(dir, "kenward.yaml"))
	t.Log("\n" + string(data))
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	t.Log("\n" + string(env))
}
