package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// implementationDoc is the build contract, and §12 lists the tests that need equipment.
const implementationDoc = "IMPLEMENTATION.md"

// buildTag matches a //go:build line's constraint text.
var buildTag = regexp.MustCompile(`^//go:build (.+)$`)

// testFileRef matches a backticked `path/to/something_test.go` in the document.
//
// A path, not a bare filename: the document also says things like "its
// `telegram_test.go`", where the directory is the sentence and resolving it would mean
// guessing which of several packages was meant. A reference that states its own path is
// making a claim that can be checked, so those are the ones checked.
var testFileRef = regexp.MustCompile("`([a-zA-Z0-9_.-]+(?:/[a-zA-Z0-9_.-]+)+_test\\.go)`")

// TestIntegrationSuiteMatchesTheDocument holds docs/IMPLEMENTATION.md's list of
// equipment-needing tests to the files that actually carry the tag.
//
// The document said there were four and named one that does not exist, while seven
// files carried `//go:build integration`. That is the worst shape a documentation error
// takes here: the list is what somebody reads to find out what a green `go test ./...`
// did *not* run, so an understated list makes the default run look like it covered more
// than it did. Nothing tied the two together, and a list maintained by memory drifts on
// the first commit that adds a tagged file without opening the document.
//
// This lives in internal/e2e because internal/e2e owns the flagship tagged test and its
// own package documentation is about what is and is not excluded from the default run.
func TestIntegrationSuiteMatchesTheDocument(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	doc, err := os.ReadFile(filepath.Join(root, "docs", implementationDoc))
	if err != nil {
		t.Fatalf("docs/%s must be readable: it lists the integration suite, and that list cannot be checked without it: %v", implementationDoc, err)
	}
	text := string(doc)

	for _, f := range taggedIntegrationFiles(t, root) {
		if !strings.Contains(text, f) {
			t.Errorf("%s carries //go:build integration and docs/%s does not name it.\n\nThe list is what tells a reader which tests a green `go test ./...` did not run; a file missing from it is a gap nobody can see.", f, implementationDoc)
		}
	}

	// The other direction. `internal/memory/integration_test.go` was named for long
	// enough that it read as a real file, and a reader who went looking for it found
	// the paragraph was describing a suite that had moved on without it.
	for _, m := range testFileRef.FindAllStringSubmatch(text, -1) {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(m[1]))); err != nil {
			t.Errorf("docs/%s names `%s`, which does not exist", implementationDoc, m[1])
		}
	}
}

// taggedIntegrationFiles returns every Go file under root whose build constraint names
// the integration tag, as slash-separated repository-relative paths.
func taggedIntegrationFiles(t *testing.T, root string) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .git holds enough loose objects to make this walk measurably slow,
			// and worktrees put whole checkouts under .claude.
			if name := d.Name(); strings.HasPrefix(name, ".") && name != "." && name != ".." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") {
				if m := buildTag.FindStringSubmatch(line); m != nil && strings.Contains(m[1], "integration") {
					rel, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					found = append(found, filepath.ToSlash(rel))
					return nil
				}
				continue
			}
			// A build constraint has to precede the package clause, so the first
			// line that is neither blank nor a comment ends the search.
			return nil
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no file carries //go:build integration; either the tag was renamed or this walk is looking in the wrong place")
	}
	return found
}
