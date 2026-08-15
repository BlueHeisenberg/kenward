package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

func testRecord(tag string) Record {
	return Record{
		Salt:       []byte("salt-" + tag),
		Params:     json.RawMessage(`{"algo":"argon2id"}`),
		WrappedDEK: []byte("wrapped-" + tag),
		SealedKey:  []byte("sealed-" + tag),
	}
}

// recordsEquivalent compares records field by field, normalising Params
// through json.Compact first: the file store's pretty-printer may re-indent
// the embedded JSON, which changes bytes without changing meaning (keel/vault
// parses Params, it never compares it).
func recordsEquivalent(t *testing.T, a, b Record) bool {
	t.Helper()
	compact := func(raw json.RawMessage) string {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			t.Fatalf("compact params: %v", err)
		}
		return buf.String()
	}
	return bytes.Equal(a.Salt, b.Salt) &&
		bytes.Equal(a.WrappedDEK, b.WrappedDEK) &&
		bytes.Equal(a.SealedKey, b.SealedKey) &&
		compact(a.Params) == compact(b.Params)
}

// storeUnderTest runs the shared Store contract tests against both
// implementations, so their behaviour cannot drift apart.
func storesUnderTest(t *testing.T) map[string]Store {
	t.Helper()
	return map[string]Store{
		"mem":  NewMemStore(),
		"file": NewFileStore(filepath.Join(t.TempDir(), "state", "keys.json")),
	}
}

func TestStoreContract(t *testing.T) {
	for name, st := range storesUnderTest(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			// Empty store: unknown member, empty list.
			if _, err := st.Load(ctx, "a"); !errors.Is(err, ErrUnknownMember) {
				t.Fatalf("Load on empty store: got %v, want ErrUnknownMember", err)
			}
			ids, err := st.List(ctx)
			if err != nil || len(ids) != 0 {
				t.Fatalf("List on empty store: got %v, %v", ids, err)
			}

			// Round trip.
			recA := testRecord("a")
			if err := st.Save(ctx, "a", recA); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got, err := st.Load(ctx, "a")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !recordsEquivalent(t, got, recA) {
				t.Fatalf("Load: got %+v, want %+v", got, recA)
			}

			// Duplicates are refused, never overwritten.
			if err := st.Save(ctx, "a", testRecord("a2")); !errors.Is(err, ErrDuplicateMember) {
				t.Fatalf("duplicate Save: got %v, want ErrDuplicateMember", err)
			}
			got, _ = st.Load(ctx, "a")
			if !recordsEquivalent(t, got, recA) {
				t.Fatalf("record changed by refused duplicate Save")
			}

			// Validation.
			if err := st.Save(ctx, "", testRecord("x")); err == nil {
				t.Fatal("Save with empty id succeeded")
			}
			for _, bad := range []Record{
				{Params: recA.Params, WrappedDEK: recA.WrappedDEK, SealedKey: recA.SealedKey},
				{Salt: recA.Salt, WrappedDEK: recA.WrappedDEK, SealedKey: recA.SealedKey},
				{Salt: recA.Salt, Params: recA.Params, SealedKey: recA.SealedKey},
				{Salt: recA.Salt, Params: recA.Params, WrappedDEK: recA.WrappedDEK},
			} {
				if err := st.Save(ctx, "b", bad); err == nil {
					t.Fatalf("Save accepted partial record %+v", bad)
				}
			}

			// List sees every member.
			if err := st.Save(ctx, "b", testRecord("b")); err != nil {
				t.Fatalf("Save b: %v", err)
			}
			ids, err = st.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			want := map[domain.MemberID]bool{"a": true, "b": true}
			if len(ids) != 2 || !want[ids[0]] || !want[ids[1]] || ids[0] == ids[1] {
				t.Fatalf("List: got %v, want a and b", ids)
			}
		})
	}
}

func TestMemStoreLoadReturnsCopy(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	rec := testRecord("a")
	if err := st.Save(ctx, "a", rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _ := st.Load(ctx, "a")
	got.Salt[0] ^= 0xff
	got.SealedKey[0] ^= 0xff
	again, _ := st.Load(ctx, "a")
	if !reflect.DeepEqual(again, rec) {
		t.Fatal("mutating a loaded record reached the store's internals")
	}
	// Saving must also detach from the caller's slices.
	rec2 := testRecord("b")
	if err := st.Save(ctx, "b", rec2); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rec2.SealedKey[0] ^= 0xff
	got2, _ := st.Load(ctx, "b")
	if reflect.DeepEqual(got2.SealedKey, rec2.SealedKey) {
		t.Fatal("mutating a saved record's slice reached the store's internals")
	}
}

func TestFileStorePermissionsAndAtomicity(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "keys.json")
	st := NewFileStore(path)

	if err := st.Save(ctx, "a", testRecord("a")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.Save(ctx, "b", testRecord("b")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// No temp files left behind after the rename.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Fatalf("unexpected file left in store dir: %s", e.Name())
		}
	}

	// The file on disk is complete, versioned JSON.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var f keyFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("file is not valid JSON: %v", err)
	}
	if f.Version != fileFormatVersion || len(f.Members) != 2 {
		t.Fatalf("file: version %d, %d members", f.Version, len(f.Members))
	}

	// Permission checks mean nothing on Windows; the mode bits are not real.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("key file readable beyond owner: %o", perm)
		}
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat dir: %v", err)
		}
		if perm := di.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("store dir accessible beyond owner: %o", perm)
		}
	}
}

func TestFileStoreRejectsBadFiles(t *testing.T) {
	ctx := context.Background()

	t.Run("malformed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		st := NewFileStore(path)
		if _, err := st.List(ctx); err == nil {
			t.Fatal("malformed file read as an empty store")
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.json")
		if err := os.WriteFile(path, []byte(`{"version":99,"members":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		st := NewFileStore(path)
		if _, err := st.List(ctx); err == nil {
			t.Fatal("unknown format version read as an empty store")
		}
	})

	t.Run("empty file is an empty store", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.json")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		st := NewFileStore(path)
		ids, err := st.List(ctx)
		if err != nil || len(ids) != 0 {
			t.Fatalf("empty file: got %v, %v", ids, err)
		}
	})
}
