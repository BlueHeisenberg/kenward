package routing_test

// This file is an external-package test on purpose: config imports routing, so
// these tests — which prove routing's KeyFunc seam composes with config's real
// secret resolver — cannot live in package routing without an import cycle.
//
// What is being pinned: an operator may supply an endpoint's API key as a
// file, as an environment variable, or as a systemd credential, and the value
// must reach the Authorization header through config's accessor — one owner
// for the precedence rules — without routing knowing which source it was.
// Everything is faked: no test here reads the real environment or a real
// filesystem.

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// fakeSecretFS serves secret files from a map, reporting mode 0600 so the
// permission check passes for present files.
type fakeSecretFS map[string]string

func (f fakeSecretFS) ReadSecretFile(path string) ([]byte, fs.FileMode, error) {
	v, ok := f[path]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	return []byte(v), 0o600, nil
}

// noEnv is an environment with nothing in it.
func noEnv(string) (string, bool) { return "", false }

func TestKeyFuncComposesWithConfigSecrets(t *testing.T) {
	credsDir := filepath.Join("run", "credentials", "kenward.service")
	cases := []struct {
		name string
		ec   config.EndpointConfig
		opts config.SecretOptions
		want string
	}{
		{
			name: "file source, trailing newline trimmed",
			ec:   config.EndpointConfig{Name: "openrouter", APIKeyFile: "/etc/kenward/or.key"},
			opts: config.SecretOptions{
				LookupEnv: noEnv,
				FS:        fakeSecretFS{"/etc/kenward/or.key": "file-key\n"},
				FileMode:  config.FileModeEnforce,
			},
			want: "file-key",
		},
		{
			name: "environment source",
			ec:   config.EndpointConfig{Name: "openrouter", APIKeyEnv: "OPENROUTER_API_KEY"},
			opts: config.SecretOptions{
				LookupEnv: func(name string) (string, bool) {
					if name == "OPENROUTER_API_KEY" {
						return "env-key", true
					}
					return "", false
				},
				FS:       fakeSecretFS{},
				FileMode: config.FileModeEnforce,
			},
			want: "env-key",
		},
		{
			name: "systemd credential, no configuration at all",
			ec:   config.EndpointConfig{Name: "openrouter"},
			opts: config.SecretOptions{
				LookupEnv: noEnv,
				FS: fakeSecretFS{
					filepath.Join(credsDir, config.EndpointAPIKeyCredential("openrouter")): "credential-key",
				},
				CredentialsDir: credsDir,
				FileMode:       config.FileModeEnforce,
			},
			want: "credential-key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAuth := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer srv.Close()

			// The production wiring shape: the resolver closes over config's
			// Secrets and endpoint entries, calls the accessor at the moment
			// of use, and hands routing only the value.
			secrets := config.NewSecrets(tc.opts)
			endpoints := []config.EndpointConfig{tc.ec}
			resolver := func(ep routing.Endpoint) (string, error) {
				for _, ec := range endpoints {
					if ec.Name != ep.Name {
						continue
					}
					s, err := ec.APIKey(secrets)
					if err != nil {
						return "", err
					}
					return s.Value(), nil
				}
				return "", nil
			}

			c := routing.NewHTTPCompleter(nil, resolver, nil)
			e := routing.Endpoint{Name: tc.ec.Name, BaseURL: srv.URL, Model: "m", Timeout: time.Second}
			if _, err := c.Complete(context.Background(), e, routing.Request{
				Messages: []routing.Message{{Role: "user", Content: "hi"}},
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if want := "Bearer " + tc.want; gotAuth != want {
				t.Fatalf("Authorization = %q, want %q", gotAuth, want)
			}
		})
	}
}
