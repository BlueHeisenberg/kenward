package enrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// sampleCode is a well-formed record; hash is the only field the store looks at.
func sampleCode(digest string, expires time.Time) Code {
	return Code{
		Hash:      digest + strings.Repeat("0", hashLen*2-len(digest)),
		Name:      "David",
		MemberID:  "david",
		IssuedAt:  epoch,
		ExpiresAt: expires,
	}
}

// stores returns a fresh instance of every Store implementation, so the contract is
// tested once and both implementations have to satisfy it.
func stores(t *testing.T) map[string]Store {
	t.Helper()
	return map[string]Store{
		"mem":  NewMemStore(),
		"file": NewFileStore(filepath.Join(t.TempDir(), "sub", "codes.json")),
	}
}

func TestStoreContract(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			live := sampleCode("aa", epoch.Add(time.Hour))
			gone := sampleCode("bb", epoch.Add(-time.Minute))

			for _, c := range []Code{live, gone} {
				if err := s.Save(ctx, c); err != nil {
					t.Fatalf("Save: %v", err)
				}
			}
			if err := s.Save(ctx, live); !errors.Is(err, ErrDuplicateCode) {
				t.Errorf("re-saving a digest: %v, want ErrDuplicateCode", err)
			}
			if err := s.Save(ctx, Code{Hash: "short"}); !errors.Is(err, ErrInvalidCode) {
				t.Errorf("saving a malformed record: %v, want ErrInvalidCode", err)
			}

			if _, err := s.Consume(ctx, sampleCode("cc", epoch).Hash, epoch); !errors.Is(err, ErrUnknownCode) {
				t.Errorf("unknown digest: %v, want ErrUnknownCode", err)
			}
			if _, err := s.Consume(ctx, gone.Hash, epoch); !errors.Is(err, ErrCodeExpired) {
				t.Errorf("expired digest: %v, want ErrCodeExpired", err)
			}

			got, err := s.Consume(ctx, live.Hash, epoch)
			if err != nil {
				t.Fatalf("Consume: %v", err)
			}
			if !got.Consumed() || !got.ConsumedAt.Equal(epoch) {
				t.Errorf("consumed code = %+v, want ConsumedAt %v", got, epoch)
			}
			if got.Name != "David" || got.MemberID != "david" {
				t.Errorf("consumed code lost its identity: %+v", got)
			}
			if _, err := s.Consume(ctx, live.Hash, epoch); !errors.Is(err, ErrCodeConsumed) {
				t.Errorf("second Consume: %v, want ErrCodeConsumed", err)
			}

			if err := s.Purge(ctx, epoch.Add(2*time.Hour)); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			if _, err := s.Consume(ctx, live.Hash, epoch); !errors.Is(err, ErrUnknownCode) {
				t.Errorf("purged code: %v, want ErrUnknownCode", err)
			}
		})
	}
}

func TestStoreHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if err := s.Save(ctx, sampleCode("aa", epoch.Add(time.Hour))); !errors.Is(err, context.Canceled) {
				t.Errorf("Save: %v, want context.Canceled", err)
			}
			if _, err := s.Consume(ctx, sampleCode("aa", epoch).Hash, epoch); !errors.Is(err, context.Canceled) {
				t.Errorf("Consume: %v, want context.Canceled", err)
			}
			if err := s.Purge(ctx, epoch); !errors.Is(err, context.Canceled) {
				t.Errorf("Purge: %v, want context.Canceled", err)
			}
		})
	}
}

func TestFileStorePersistsAcrossInstances(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "codes.json")
	c := sampleCode("aa", epoch.Add(time.Hour))

	if err := NewFileStore(path).Save(ctx, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := NewFileStore(path).Consume(ctx, c.Hash, epoch)
	if err != nil {
		t.Fatalf("Consume from a second instance: %v", err)
	}
	if got.MemberID != "david" {
		t.Errorf("record did not survive the round trip: %+v", got)
	}
	if _, err := NewFileStore(path).Consume(ctx, c.Hash, epoch); !errors.Is(err, ErrCodeConsumed) {
		t.Errorf("consumption did not survive the round trip: %v", err)
	}
}

func TestFileStoreMissingFileIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(filepath.Join(t.TempDir(), "nothing-here.json"))
	if _, err := s.Consume(ctx, sampleCode("aa", epoch).Hash, epoch); !errors.Is(err, ErrUnknownCode) {
		t.Errorf("missing file: %v, want ErrUnknownCode", err)
	}
	if err := s.Purge(ctx, epoch); err != nil {
		t.Errorf("Purge on a missing file: %v", err)
	}
}

// TestFileStoreRefusesAMalformedFile: starting over from empty would silently
// invalidate every outstanding invite, and look like nothing had happened.
func TestFileStoreRefusesAMalformedFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	for name, content := range map[string]string{
		"not json":        "{ this is not json",
		"wrong version":   `{"version":99,"codes":[]}`,
		"missing version": `{"codes":[]}`,
		"empty file":      "",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			s := NewFileStore(path)
			if _, err := s.Consume(ctx, sampleCode("aa", epoch).Hash, epoch); err == nil {
				t.Error("Consume accepted a malformed file")
			}
			if err := s.Save(ctx, sampleCode("aa", epoch.Add(time.Hour))); err == nil {
				t.Error("Save overwrote a malformed file")
			}
		})
	}
}

func TestFileStorePermissions(t *testing.T) {
	// Nested, so the directory under test is one the store created rather than the
	// world-readable one the test framework handed us.
	path := filepath.Join(t.TempDir(), "state", "codes.json")
	if err := NewFileStore(path).Save(context.Background(), sampleCode("aa", epoch.Add(time.Hour))); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS == "windows" {
		// Windows has no POSIX mode bits; Go synthesises 0666/0444 from the
		// read-only attribute, so the assertion below cannot mean anything there.
		// The file still lives in the container's own data directory.
		t.Skipf("mode bits are not meaningful on %s (got %v)", runtime.GOOS, fi.Mode().Perm())
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("code file mode = %04o, want 0600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("code directory mode = %04o, want no group or world bits", got)
	}
}

// TestFileStoreWritesAtomically checks the observable consequences of the temp-write
// and rename: no partial file is ever left behind, and no temp files accumulate.
func TestFileStoreWritesAtomically(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "codes.json")
	s := NewFileStore(path)

	for i := 0; i < 20; i++ {
		if err := s.Save(ctx, sampleCode(strings.Repeat("a", i+1), epoch.Add(time.Hour))); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "codes.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only codes.json", names)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f codeFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("file is not valid JSON after 20 writes: %v", err)
	}
	if f.Version != fileFormatVersion || len(f.Codes) != 20 {
		t.Fatalf("file = version %d, %d codes; want version %d, 20 codes", f.Version, len(f.Codes), fileFormatVersion)
	}
	if strings.Contains(string(b), "plaintext") {
		t.Error("file holds a field named plaintext")
	}
}

// TestFileStoreConcurrentWrites hammers one file from many goroutines: every save
// must land and the file must stay parseable throughout.
func TestFileStoreConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "codes.json")
	s := NewFileStore(path)

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Save(ctx, sampleCode(fmt.Sprintf("f%04d", i), epoch.Add(time.Hour))); err != nil {
				t.Errorf("Save %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f codeFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("file is not valid JSON: %v", err)
	}
	if len(f.Codes) != n {
		t.Errorf("file holds %d codes, want %d", len(f.Codes), n)
	}
}

func TestCodeStateHelpers(t *testing.T) {
	c := sampleCode("aa", epoch.Add(time.Hour))
	if c.Consumed() {
		t.Error("fresh code reports Consumed")
	}
	if c.Expired(epoch) || !c.Live(epoch) {
		t.Error("fresh code is not live")
	}
	if !c.Expired(epoch.Add(time.Hour)) {
		t.Error("expiry boundary is exclusive; it must be inclusive")
	}
	if c.Live(epoch.Add(time.Hour)) {
		t.Error("expired code reports Live")
	}
	c.ConsumedAt = epoch
	if !c.Consumed() || c.Live(epoch) {
		t.Error("consumed code reports Live")
	}
}
