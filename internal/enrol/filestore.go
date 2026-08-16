package enrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// fileFormatVersion is written into the file so a future format change can be
// detected rather than mis-parsed.
const fileFormatVersion = 1

// codeFile is the on-disk shape. The wrapper object exists so the top level is a
// JSON object and can gain fields without breaking older readers.
type codeFile struct {
	Version int    `json:"version"`
	Codes   []Code `json:"codes"`
}

// FileStore is a Store backed by a single JSON file.
//
// The file holds digests and expiry times, nothing secret in itself, but it is
// written 0600 anyway: knowing which invites are still outstanding is knowing where
// to aim; see Readable for the one copy that is not, and why.
//
// Every write goes to a temp file in the same directory, is fsynced, and is
// then renamed over the target, so a crash mid-write leaves the previous file
// intact rather than a truncated one. There is no partially written state a reader
// can observe.
//
// Read-modify-write cycles run under a mutex and re-read the file each time, so
// Consume is atomic against every other operation in the process. It is not atomic
// against a second process writing the same path, and is not meant to be: kenward
// runs one container per household.
//
// The zero value is not usable; call NewFileStore.
type FileStore struct {
	mu   sync.Mutex
	path string
	mode fs.FileMode
}

// NewFileStore returns a store backed by the file at path. The file and its parent
// directory are created on first write; a path that does not exist yet reads as an
// empty store rather than an error.
func NewFileStore(path string) *FileStore { return &FileStore{path: path, mode: 0o600} }

// Readable makes the store's file 0644 instead of 0600, and returns it so the change
// reads as part of the construction.
//
// It exists for one case and should not be used for another: a copy of some records
// written on one side of a container boundary to be read on the other. A bind-mounted
// 0600 file carries its owner across the mount, and the process on the far side runs
// as a different user id — the image's fixed non-root account — so it cannot open the
// file at all. Chowning instead is not an answer that holds: this file is rewritten on
// every mint, so the ownership would have to be restored each time, and under a
// rootless runtime the id it would have to be set to is not the id the container sees.
//
// What it costs is bounded by where these files live. The parent directory is created
// 0700, so on the host the set of accounts that can reach the file is unchanged; what
// changes is only that a process inside the container it was written for can read it.
// It must never be used for the session store or for a store that holds anything
// redeemable — see the package documentation on what a record is.
func (s *FileStore) Readable() *FileStore { s.mode = 0o644; return s }

// Path reports the file the store is backed by.
func (s *FileStore) Path() string { return s.path }

// All returns every record the store holds, consumed and expired ones included.
//
// It exists for one caller: moving a member's outstanding invites from the store the
// operator minted into onto the store their pod will redeem against, which are two
// files on two filesystems in isolated mode. Every record is a digest, so what
// travels is unredeemable in itself — see the package documentation and Code.
//
// It is deliberately not on Store. Reading the whole file is not something enrolment
// needs; it is something a deployment needs, and putting it on the interface would
// invite a caller to scan for a code rather than Consume one, which is the timing
// leak EqualHash exists to close.
func (s *FileStore) All(ctx context.Context) ([]Code, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Save records a newly minted code.
func (s *FileStore) Save(ctx context.Context, c Code) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	codes, err := s.load()
	if err != nil {
		return err
	}
	for i := range codes {
		if EqualHash(codes[i].Hash, c.Hash) {
			return ErrDuplicateCode
		}
	}
	return s.store(append(codes, c))
}

// Consume atomically redeems a code by digest. The record is marked consumed and
// the file replaced before the code is returned, so a crash between the two cannot
// hand out a code that still reads as live.
func (s *FileStore) Consume(ctx context.Context, digest string, now time.Time) (Code, error) {
	if err := ctx.Err(); err != nil {
		return Code{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	codes, err := s.load()
	if err != nil {
		return Code{}, err
	}
	idx, c, err := consume(codes, digest, now)
	if err != nil {
		return Code{}, err
	}
	codes[idx] = c
	if err := s.store(codes); err != nil {
		return Code{}, err
	}
	return c, nil
}

// Purge drops codes finished with before the given time.
func (s *FileStore) Purge(ctx context.Context, before time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	codes, err := s.load()
	if err != nil {
		return err
	}
	kept := purge(codes, before)
	if len(kept) == len(codes) {
		return nil
	}
	return s.store(kept)
}

// load reads the file. A missing file is an empty store; a malformed one is an
// error, never an empty store, because silently starting over would quietly
// invalidate every outstanding invite.
func (s *FileStore) load() ([]Code, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("enrol: read %s: %w", s.path, err)
	}
	var f codeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("enrol: parse %s: %w", s.path, err)
	}
	if f.Version != fileFormatVersion {
		return nil, fmt.Errorf("enrol: %s: unsupported format version %d", s.path, f.Version)
	}
	return f.Codes, nil
}

// store writes the codes: temp file in the same directory, fsync, rename over the
// target. Same directory matters — rename is only atomic within a filesystem.
func (s *FileStore) store(codes []Code) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("enrol: create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(codeFile{Version: fileFormatVersion, Codes: codes}, "", "  ")
	if err != nil {
		return fmt.Errorf("enrol: encode codes: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".codes-*.tmp")
	if err != nil {
		return fmt.Errorf("enrol: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	mode := s.mode
	if mode == 0 {
		mode = 0o600
	}
	if err := tmp.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return fmt.Errorf("enrol: chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("enrol: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("enrol: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("enrol: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("enrol: replace %s: %w", s.path, err)
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so the rename itself is durable, not just the bytes it
// points at. Best effort: not every platform lets a directory be opened for sync,
// and on those the rename is durable by other means or not at all — either way
// failing the write over it would be worse than the exposure.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
