package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestTheIdentityStepDoesNotContradictItsOwnRefusal.
//
// The wizard refuses per_member in simple mode, with a good message. Three lines below
// that banner, on the same screen, the explanatory copy read "It costs nothing, it
// needs no containers, and either answer works in either mode." — which is false, and
// is the opposite of what the banner immediately above it says.
func TestTheIdentityStepDoesNotContradictItsOwnRefusal(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "simple")

	resp := h.postCSRF("/setup/advanced", url.Values{
		"agents":         {"per_member"},
		"update_channel": {"stable"},
		"idle_timeout":   {"0s"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// Collapsed, because the template wraps its prose and a sentence split over two
	// source lines is the same false sentence on the screen.
	page := strings.Join(strings.Fields(body(t, resp)), " ")
	if strings.Contains(page, "either answer works in either mode") {
		t.Errorf("the page still says either answer works in either mode, three lines under a banner refusing one of them:\n%s", page)
	}
	// And it says the true thing in its place, on the screen where the choosing
	// happens rather than only in the refusal.
	if !strings.Contains(page, "isolated mode") {
		t.Errorf("the identity copy no longer says which mode one each needs:\n%s", page)
	}
}

// TestTheReviewStepShowsTheContextWindowThatWillBeWritten.
//
// "Ask each machine" is optional, and the operator who skips it gets whatever the
// endpoints step's blank box means — kenward's own modest default. That is by design;
// what was not by design is that the number never appeared on any screen. A 262144-token
// machine configured at 16384 is a machine bought for its window and wasted silently, and
// review is the last screen before it is committed.
func TestTheReviewStepShowsTheContextWindowThatWillBeWritten(t *testing.T) {
	h := newHarness(t)
	wizardTo(t, h, "simple")

	resp := h.postCSRF("/setup/advanced", url.Values{
		"update_channel": {"stable"},
		"idle_timeout":   {"0s"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("advanced step: status = %d, want 303", resp.StatusCode)
	}

	review := h.get("/setup/review")
	defer review.Body.Close()
	page := body(t, review)
	// wizardTo never presses "Ask each machine", so nothing is known and the default
	// is what will land in the file. Review must say which number that is.
	if !strings.Contains(page, "16384") {
		t.Errorf("the review screen does not show the context window that will be written:\n%s", page)
	}
}
