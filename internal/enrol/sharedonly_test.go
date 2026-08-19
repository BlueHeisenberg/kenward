package enrol

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
)

// TestSharedOnlyOnboardingPromisesNoPrivateMemory is the defect this feature would
// have shipped with if nobody looked.
//
// The first message every enrolling member reads opens "This chat is private — just
// you and me — is your private memory. What you tell me here is stored in your own
// space, and the household group can never read it." Every clause of that is false
// for a member with no space of their own: what they tell kenward in that chat is
// exactly what goes to the household's shared memory, where the group reads it. It is
// the same class of defect as the sealed-memory language that used to appear under
// simple mode — a claim true of the household and false about one of the people in it
// — and it is worse in one respect, because a member acting on it would deliberately
// take something to the private chat in order to keep it out of the group.
//
// Asserted in every language, because the sentence exists in ten and the bug would be
// in whichever one a household actually reads.
func TestSharedOnlyOnboardingPromisesNoPrivateMemory(t *testing.T) {
	t.Parallel()

	modes := []privacy.Mode{privacy.ModeSimple, privacy.ModeIsolated, privacy.ModeUnknown}
	for _, tag := range lang.Tags() {
		c := lang.For(tag)
		for _, mode := range modes {
			for _, askPrivate := range []bool{false, true} {
				msgs := Explanation(42, c, askPrivate, mode, true)
				var body strings.Builder
				for _, m := range msgs {
					body.WriteString(m.Text)
					body.WriteString("\n")
				}
				got := body.String()

				// None of the three paragraphs that describe a memory of their
				// own may reach them, whole or in part.
				for name, banned := range map[string]string{
					"the private-chat promise": c.EnrolPrivateBody,
					"the sealed-memory claim":  c.EnrolPrivateSealed,
					"the private-write rule":   c.EnrolMemoryBodyDefault,
					"the ask-first rule":       c.EnrolMemoryBodyAsk,
				} {
					if banned == "" {
						continue
					}
					if strings.Contains(got, banned) {
						t.Errorf("%s/%v/askPrivate=%v: a member with no memory of their own was sent %s", tag, mode, askPrivate, name)
					}
				}
				// And what they are sent is the text written for them.
				if !strings.Contains(got, c.EnrolSharedOnlyBody) {
					t.Errorf("%s/%v/askPrivate=%v: the shared-memory explanation was not sent", tag, mode, askPrivate)
				}
			}
		}
	}
}

// TestSharedOnlyOnboardingIsTheSameInBothModes states the fact rather than leaving it
// to be inferred from the test above.
//
// Sealing is about who can read a member's own memory, and this member has none.
// There is nothing for isolated mode to seal and nothing for simple mode to expose, so
// the honest text is one text — and a future change that made isolated mode say
// something warmer here would be making a claim about a space that does not exist.
func TestSharedOnlyOnboardingIsTheSameInBothModes(t *testing.T) {
	t.Parallel()

	for _, tag := range lang.Tags() {
		c := lang.For(tag)
		simple := Explanation(1, c, false, privacy.ModeSimple, true)
		isolated := Explanation(1, c, false, privacy.ModeIsolated, true)
		if len(simple) != len(isolated) {
			t.Fatalf("%s: %d messages in simple mode and %d in isolated", tag, len(simple), len(isolated))
		}
		for i := range simple {
			if simple[i].Text != isolated[i].Text {
				t.Errorf("%s: message %d differs between modes; a member with no memory of their own has nothing for a mode to protect", tag, i)
			}
		}
	}
}

// TestFullMemberOnboardingIsUnchanged is the other half, and the one that would catch
// a fix applied too widely: everybody else still gets exactly what they got.
func TestFullMemberOnboardingIsUnchanged(t *testing.T) {
	t.Parallel()

	for _, tag := range lang.Tags() {
		c := lang.For(tag)
		msgs := Explanation(1, c, false, privacy.ModeSimple, false)
		if len(msgs) != 3 {
			t.Fatalf("%s: a full member got %d messages, want the three they have always had", tag, len(msgs))
		}
		if !strings.Contains(msgs[0].Text, c.EnrolPrivateBody) {
			t.Errorf("%s: a full member was not told their private chat is their private memory", tag)
		}
		if strings.Contains(msgs[0].Text, c.EnrolSharedOnlyBody) {
			t.Errorf("%s: a full member was told they have no memory of their own", tag)
		}
	}
}

// TestSharedOnlyTutorialAsksNoQuestionAboutAnAgent: the three questions OneEach adds
// are what to call their agent, how it should sound and what it should be like, and
// there is no agent of theirs for any of them to describe. config.PersonaFor gives
// them the household's voice whatever they answer, so asking would be collecting
// answers nothing reads — the exact bug the tutorial's own comments record having had
// once already.
func TestSharedOnlyTutorialAsksNoQuestionAboutAnAgent(t *testing.T) {
	t.Parallel()

	c, err := New(NewMemStore(), nil, WithOneEach())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	full := c.Tutorial(nil, domain.Member{ID: "david", Name: "David", Private: "dp"}, 1, nil)
	if !full.OneEach {
		t.Fatal("a member with an agent of their own is not asked to name it")
	}
	shared := c.Tutorial(nil, domain.Member{ID: "leo", Name: "Leo", SharedOnly: true}, 2, nil)
	if shared.OneEach {
		t.Error("a member with no agent of their own was asked to name one, choose its register and describe its character")
	}
}

// TestPrivacyStatementIsQualifiedForSharedOnly: internal/privacy is where a claim
// becomes checkable, and both of its statements open by promising a member their own
// separate memory. The note that says who that is not promised to has to exist there,
// beside them, and not in whichever caller happened to think of it.
func TestSharedOnlyNoteExistsAndIsModeBlind(t *testing.T) {
	t.Parallel()

	note := privacy.SharedOnlyNote()
	if strings.TrimSpace(note) == "" {
		t.Fatal("there is no statement for a member with no memory of their own")
	}
	// The two things it has to say, and the one it may not.
	for _, want := range []string{"shared_only", "shared memory", "private space"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not mention %q", want)
		}
	}
	for _, banned := range []string{"sealed", "nobody else"} {
		if strings.Contains(strings.ToLower(note), banned) {
			t.Errorf("the note uses %q, which is a claim about a memory this member does not have", banned)
		}
	}
	// Both statements it qualifies still make the promise it is there to bound. If
	// one of them ever stops, this note becomes an answer to a question nobody
	// asked and should be revisited rather than left.
	for _, m := range []privacy.Mode{privacy.ModeSimple, privacy.ModeIsolated} {
		if !strings.Contains(strings.ToLower(privacy.Statement(m)), "private memory") {
			t.Errorf("privacy.Statement(%v) no longer promises a private memory; the shared_only note may be stale", m)
		}
	}
}
