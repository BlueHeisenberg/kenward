package setup

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// scriptedAnswers is a complete `--non-interactive` install, for the tests that vary one
// field of it.
func scriptedAnswers() *Answers {
	return &Answers{
		HouseholdName: "Casa",
		SharedSpace:   householdSpaceID,
		BotToken:      realToken,
		MemberNames:   []string{"David"},
		MemberSpaces:  map[string]string{"david": davidSpaceID},
		Endpoints: []EndpointAnswer{
			{Name: "monster", BaseURL: "http://monster.tail:8000/v1", Model: "qwen3"},
		},
	}
}

// TestScriptedPerMemberWithoutAGroupChatIsRefused: `--non-interactive` cannot produce
// the household the question refuses either. A scripted install that writes an
// unreachable household is the same defect with nobody at the terminal to notice.
func TestScriptedPerMemberWithoutAGroupChatIsRefused(t *testing.T) {
	answers := scriptedAnswers()
	answers.Mode = config.ModeIsolated
	answers.Agents = config.AgentsPerMember

	_, _, _, err := runWizard(t, "linux", Options{Answers: answers})
	if err == nil {
		t.Fatal("a scripted install wrote one assistant each with no group chat id")
	}
	if !strings.Contains(err.Error(), "group_chat_id") {
		t.Errorf("the refusal does not name the missing field: %v", err)
	}

	answers.GroupChatID = -1001234567890
	_, cfg, _, err := runWizard(t, "linux", Options{Answers: answers})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if cfg.Household.GroupChatID != -1001234567890 {
		t.Errorf("household.group_chat_id = %d, want the answer given", cfg.Household.GroupChatID)
	}
}
