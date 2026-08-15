package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// fileFormatVersion is written into the file so a future format change can be
// detected rather than mis-parsed.
const fileFormatVersion = 1

// memberRecord pairs a Record with the member it belongs to, for persistence
// only. The id lives here — in the file's index — and not in Record itself,
// so nothing loaded from disk can supply the identity used as AAD; that
// always comes from the caller.
type memberRecord struct {
	ID domain.MemberID `json:"id"`
	Record
}

// keyFile is the on-disk shape. The wrapper object exists so the top level is
// a JSON object and can gain fields without breaking older readers.
type keyFile struct {
	Version int            `json:"version"`
	Members []memberRecord `json:"members"`
}

// FileStore is a Store backed by a single JSON file.
//
// The file holds wrapped keys — recoverable only with a passphrase, but worth
// exactly one offline guessing campaign each — so it is written 0600 and its
// directory created 0700. Every write goes to a temp file in the same
// directory, is fsynced, and is then renamed over the target, so a crash
// mid-write leaves the previous file intact rather than a truncated one.
// There is no partially written state a reader can observe.
//
// Read-modify-write cycles run under a mutex and re-read the file each time,
// so Save is atomic against every other operation in the process. It is not
// atomic against a second process writing the same path, and is not meant to
// be: kenward runs one container per household.
//
// The zero value is not usable; call NewFileStore.
type FileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore returns a store backed by the file at path. The file and its
// parent directory are created on first write; a path that does not exist yet
// reads as an empty store rather than an error.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Path reports the file the store is backed by.
func (s *FileStore) Path() string { return s.path }

// Save records a newly provisioned wrapped key.
func (s *FileStore) Save(ctx context.Context, id domain.MemberID, r Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSave(id, r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.load()
	if err != nil {
		return err
	}
	for i := range recs {
		if recs[i].ID == id {
			return ErrDuplicateMember
		}
	}
	return s.store(append(recs, memberRecord{ID: id, Record: r}))
}

// Load returns the member's wrapped key, or ErrUnknownMember.
func (s *FileStore) Load(ctx context.Context, id domain.MemberID) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.load()
	if err != nil {
		return Record{}, err
	}
	for i := range recs {
		if recs[i].ID == id {
			return recs[i].Record, nil
		}
	}
	return Record{}, ErrUnknownMember
}

// List returns the ids of every member holding a wrapped key.
func (s *FileStore) List(ctx context.Context) ([]domain.MemberID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.load()
	if err != nil {
		return nil, err
	}
	ids := make([]domain.MemberID, 0, len(recs))
	for i := range recs {
		ids = append(ids, recs[i].ID)
	}
	return ids, nil
}

// load reads the file. A missing file is an empty store; a malformed one is
// an error, never an empty store, because silently starting over would
// quietly lock every member out of their memory.
func (s *FileStore) load() ([]memberRecord, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: read %s: %w", s.path, err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var f keyFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("session: parse %s: %w", s.path, err)
	}
	if f.Version != fileFormatVersion {
		return nil, fmt.Errorf("session: %s: unsupported format version %d", s.path, f.Version)
	}
	return f.Members, nil
}

// store writes the records: temp file in the same directory, fsync, rename
// over the target. Same directory matters — rename is only atomic within a
// filesystem.
func (s *FileStore) store(recs []memberRecord) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("session: create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(keyFile{Version: fileFormatVersion, Members: recs}, "", "  ")
	if err != nil {
		return fmt.Errorf("session: encode records: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".keys-*.tmp")
	if err != nil {
		return fmt.Errorf("session: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return fmt.Errorf("session: chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("session: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("session: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("session: replace %s: %w", s.path, err)
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so the rename itself is durable, not just the
// bytes it points at. Best effort: not every platform lets a directory be
// opened for sync, and on those the rename is durable by other means or not
// at all — either way failing the write over it would be worse than the
// exposure.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
