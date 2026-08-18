package setup

import (
	"os"
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

// TestScriptedPerMemberInSimpleModeIsRefused: the other half of the identity question's
// refusal, and the one a scripted install could reach first.
//
// The terminal wizard prints identityNeedsIsolated and asks again; the dashboard refuses
// in the form. Here it is refused before any lore space is created — an install script
// that has just been told it cannot have one assistant each should not also be left with
// two spaces it did not ask for.
func TestScriptedPerMemberInSimpleModeIsRefused(t *testing.T) {
	answers := scriptedAnswers()
	answers.Mode = config.ModeSimple
	answers.Agents = config.AgentsPerMember
	answers.GroupChatID = -1001234567890

	_, _, _, err := runWizard(t, "linux", Options{Answers: answers})
	if err == nil {
		t.Fatal("a scripted install wrote one assistant each behind one household bot")
	}
	// The reason, not just the refusal: this is a counting problem, and an operator
	// told only "invalid" will try the same thing again with a different flag.
	if !strings.Contains(err.Error(), string(config.ModeIsolated)) {
		t.Errorf("the refusal does not name the mode that can deliver it: %v", err)
	}
	// And not the message that says this is somebody else's bug.
	if strings.Contains(err.Error(), "bug in setup") {
		t.Errorf("a scripted install's own answers were reported as a defect in setup: %v", err)
	}
}

// TestScriptedPerMemberWritesAStartableHousehold is the D7 counterpart of the
// dashboard's TestIsolatedWizardWritesAStartableHousehold, and it is the one that
// matters: it walks the flagship privacy mode's happy path through the scripted entry
// point and then judges the file that was written, against the environment the wizard
// itself says the operator has to create.
//
// Before `--agents`, `--group-chat-id` and the persona flags existed no scripted install
// could produce this household at all — `agents: per_member` was unreachable from a
// script, and the group's pod, which is the only place kenward speaks under one
// assistant each, had no chat id to serve. internal/supervisor refuses that outright:
// "supervisor: group unit selected but no group chat is configured".
func TestScriptedPerMemberWritesAStartableHousehold(t *testing.T) {
	answers := scriptedAnswers()
	answers.Mode = config.ModeIsolated
	answers.Agents = config.AgentsPerMember
	answers.GroupChatID = -1001234567890
	answers.Persona = config.PersonaConfig{Language: "Spanish", Tone: "warm", Character: "Knows the house well."}
	answers.MemberNames = []string{"David", "Robin"}
	answers.MemberSpaces = map[string]string{"david": davidSpaceID}

	w, _, _, err := runWizard(t, "linux", Options{Answers: answers})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Re-read what was written rather than trusting the value in memory: the file is
	// what the household starts from.
	f, err := os.Open(w.ConfigPath())
	if err != nil {
		t.Fatalf("the wizard reported success and wrote nothing: %v", err)
	}
	defer f.Close()
	cfg, err := config.Decode(f)
	if err != nil {
		t.Fatalf("the wizard wrote a file that does not parse: %v", err)
	}
	if err := cfg.Validate(w.validationEnv()); err != nil {
		t.Fatalf("the wizard wrote a household that will not start from the environment it named:\n%v", err)
	}

	if !cfg.AgentPerMember() {
		t.Errorf("household.agents = %q, want %q: the whole third-scope design is what the flag exists for",
			cfg.Household.Agents, config.AgentsPerMember)
	}
	// The group pod's precondition, in the file. Zero here is the configuration
	// internal/supervisor refuses to start a group unit from.
	if cfg.Household.GroupChatID == 0 {
		t.Error("household.group_chat_id is unset: the group pod cannot start, and under one assistant each it is the only place kenward speaks")
	}
	if got := cfg.Household.Persona; got != answers.Persona {
		t.Errorf("household.persona = %+v, want %+v", got, answers.Persona)
	}
	// Every member has their own two variables named, which is what the group pod's
	// siblings need. No value is supplied by a scripted install and none should be:
	// setup names the variables, the script exports them.
	for _, m := range cfg.Members {
		if m.BotTokenEnv == "" || m.PassphraseEnv == "" {
			t.Errorf("member %s has no own secret variable in an isolated household", m.ID)
		}
	}
	data, err := os.ReadFile(w.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "lore_command") {
		t.Errorf("an isolated configuration still names a lore binary nothing runs:\n%s", data)
	}
}
