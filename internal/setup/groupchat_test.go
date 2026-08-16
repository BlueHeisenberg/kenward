package setup

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// The household group's chat id, asked for only under one assistant each.
//
// Under `agents: per_member` kenward itself lives in the group chat and nowhere else —
// every private chat belongs to somebody's own assistant — and the supervisor creates
// the group's pod only when household.group_chat_id is set
// (internal/supervisor/isolated.go). So a per_member configuration with no group chat id
// is a household with no kenward in it at all, in the group or anywhere, and the wizard
// that wrote it told nobody.

// oneEachScript is a full isolated-mode run that answers "one each", with whatever the
// caller supplies for the group chat question in between the choice and the persona.
//
// Called with nothing, it is exactly the script the wizard consumed before the group
// chat was asked for — which is what makes TestPerMemberFromTheWizardIsReachable a test
// of the defect rather than of the fix.
func oneEachScript(t *testing.T, groupAnswers ...string) []string {
	t.Helper()
	answers := append([]string{"2"}, simpleAnswers()[1:]...)
	for i, a := range answers {
		// The identity step is marked in simpleAnswers by four empty answers in a row.
		if a == "" && i+3 < len(answers) && answers[i+1] == "" && answers[i+2] == "" && answers[i+3] == "" {
			out := append([]string(nil), answers[:i]...)
			out = append(out, "2")
			out = append(out, groupAnswers...)
			out = append(out, "", "", "")
			return append(out, answers[i+4:]...)
		}
	}
	t.Fatal("the answer script no longer has the identity step's four defaults in a row")
	return nil
}

// TestPerMemberFromTheWizardIsReachable is the test that would have caught the original.
//
// It gives the wizard the answers it used to take — one assistant each, and nothing at
// all about a group — and asserts the one property a written configuration has to have:
// that there is somewhere for kenward to be. Whatever the wizard does with those
// answers, it may not finish by writing a household that cannot be reached.
func TestPerMemberFromTheWizardIsReachable(t *testing.T) {
	_, cfg, io, err := runWizard(t, "linux", Options{}, oneEachScript(t)...)
	if err != nil {
		// Refusing is a perfectly good answer: nothing was written.
		return
	}
	if cfg.Household.Agents == config.AgentsPerMember && cfg.Household.GroupChatID == 0 {
		t.Fatalf("the wizard wrote household.agents = %q with household.group_chat_id unset.\n"+
			"Under one assistant each, kenward speaks only in the group chat — every private chat "+
			"belongs to somebody's own assistant — and the supervisor creates the group's pod only "+
			"when group_chat_id is set. This household has no kenward in it, in the group or "+
			"anywhere, and setup said nothing.\n%s",
			cfg.Household.Agents, io.Transcript())
	}
}

// TestOneEachAsksForTheGroupChatAndRefusesABlankOne: the question is put, a blank answer
// is refused with the reason, and the id that is finally given reaches the file.
//
// Refused rather than warned about. `kenward doctor` already warns, and the warning did
// not help: nothing in the wizard tells anybody to run doctor.
func TestOneEachAsksForTheGroupChatAndRefusesABlankOne(t *testing.T) {
	_, cfg, io, err := runWizard(t, "linux", Options{}, oneEachScript(t, "", "-1001234567890")...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if io.Remaining() != 0 {
		t.Errorf("%d scripted answers were never used, so the flow is not the one under test", io.Remaining())
	}
	if cfg.Household.GroupChatID != -1001234567890 {
		t.Errorf("household.group_chat_id = %d, want -1001234567890", cfg.Household.GroupChatID)
	}
	transcript := io.Transcript()
	if !strings.Contains(transcript, "no kenward in it") {
		t.Errorf("a blank group chat id was refused without saying what it costs:\n%s", transcript)
	}
	// And it says where the number comes from, because this is the one numeric Telegram
	// id the wizard asks for and there is no second route to it.
	if !strings.Contains(transcript, "getUpdates") {
		t.Errorf("the group chat question does not say how to find the id:\n%s", transcript)
	}
}

// TestOneSharedAssistantIsNeverAskedForAGroupChat. The question is only load-bearing
// under one assistant each: with one shared assistant kenward answers every private chat
// whether or not a group is mapped, and asking for a numeric Telegram id nobody needs
// would be the wizard breaking its own rule about them.
func TestOneSharedAssistantIsNeverAskedForAGroupChat(t *testing.T) {
	_, cfg, io, err := runWizard(t, "linux", Options{}, simpleAnswers()...)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, io.Transcript())
	}
	if cfg.Household.GroupChatID != 0 {
		t.Errorf("household.group_chat_id = %d, want 0 from a household that was never asked", cfg.Household.GroupChatID)
	}
	if strings.Contains(io.Transcript(), "group's chat id") {
		t.Errorf("one shared assistant was asked for a group chat id:\n%s", io.Transcript())
	}
}
