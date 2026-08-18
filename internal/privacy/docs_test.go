package privacy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docsClaimingTheSeal are the published surfaces that restate isolated mode's at-rest
// claim. They are not code and nothing compiles them, which is exactly why they need a
// test: the golden tests on this package protected the statement kenward prints and left
// five copies of it in the README, on the landing page and in two documents, where the
// pre-D-019 wording sat live on a published site for as long as nobody reread it.
var docsClaimingTheSeal = []string{
	"README.md",
	filepath.Join("site", "index.html"),
	filepath.Join("docs", "ARCHITECTURE.md"),
	filepath.Join("docs", "INSTALL.md"),
}

// sealClause is the strongest true form of the at-rest claim, and it is this package's
// own words rather than a reasonable paraphrase of them.
//
// The paraphrase is the failure mode. Every one of the five copies this test now covers
// began as an accurate restatement of internal/privacy and ended up saying "not outside
// your session", "while you are not in session" or "while that member is away" — D-010's
// wording, which D-019 withdrew because it cannot be honoured. Nothing caught it, because
// nothing tied the documents to the package. Quotation is the tie: soften the statement
// and this fails, embellish a document and this fails.
const sealClause = "not from the disk, not from a backup, and not before your process has been unlocked"

// withdrawnClaims are the shapes in which the retired promise was actually *made*.
//
// The list is of assertions, not of the words. A document is expected to name what was
// withdrawn — a claim that weakens without saying so is worse than one never made — so
// the ban is on the constructions that state the thing, and the positive check above is
// what catches a rewording nobody anticipated.
var withdrawnClaims = []string{
	"not outside your session",
	"while you are not in session",
	"or while that member is away",
	"or while you are away",
}

// TestPublishedDocsQuoteTheSealClause holds the documents to internal/privacy's wording.
func TestPublishedDocsQuoteTheSealClause(t *testing.T) {
	t.Parallel()

	// This package's half of the contract first. If the statement stops saying it, the
	// documents are quoting something that no longer exists and every assertion below
	// would be measuring a copy against nothing.
	if !strings.Contains(flatten(Statement(ModeIsolated)), sealClause) {
		t.Fatalf("privacy.Statement(ModeIsolated) no longer contains %q — four published documents quote it, so either the statement moved or they must", sealClause)
	}

	for _, name := range docsClaimingTheSeal {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Not a skip. A document that cannot be read is a failure: a guard that
			// stands down when it cannot find its subject is no guard at all.
			b, err := os.ReadFile(filepath.Join("..", "..", name))
			if err != nil {
				t.Fatalf("%s must be readable: it restates internal/privacy's claim, and that cannot be checked without it: %v", name, err)
			}
			doc := flatten(string(b))

			if !strings.Contains(doc, sealClause) {
				t.Errorf("%s no longer quotes internal/privacy's at-rest claim.\nwant it to contain: %q\n\nOne of the two was edited without the other. The package is the contract; fix whichever is wrong, deliberately.", name, sealClause)
			}
			for _, claim := range withdrawnClaims {
				if strings.Contains(strings.ToLower(doc), claim) {
					t.Errorf("%s states %q — D-019 withdrew that claim, because a key is not re-locked after a quiet spell and re-unlocking one would mean sending a passphrase over Telegram", name, claim)
				}
			}
		})
	}
}
