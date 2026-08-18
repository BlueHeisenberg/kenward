package main

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// minimalFlags is the smallest `--non-interactive` invocation that builds, for the
// tests that vary one flag of it.
func minimalFlags() *setupFlags {
	return &setupFlags{
		mode:        "simple",
		household:   "Casa",
		botTokenEnv: "KENWARD_BOT_TOKEN",
		members:     stringList{"David"},
		endpoints:   stringList{"name=m,url=http://m:8000/v1,model=q"},
	}
}

// TestBuildAnswersCarriesSpaceIDs.
//
// A scripted install has to be able to say which lore space is whose, and it has to
// be able to say it with an id: spaces are resolved by id, and a display name here
// writes memory happily and returns nothing on the first read. The values are passed
// through unchecked on purpose — internal/setup validates them against lore's real
// listing, and a second, weaker check here would let --non-interactive write a
// configuration the interactive wizard would have refused.
func TestBuildAnswersCarriesSpaceIDs(t *testing.T) {
	t.Parallel()
	const (
		shared = "dac31e70-72e4-4b10-9cef-a6276c4a87b8"
		david  = "7d5047bb-d939-4539-b3db-8b6221a2e245"
	)
	f := minimalFlags()
	f.sharedSpace = shared
	f.memberSpaces = stringList{"david=" + david}
	a, err := buildAnswers(f)
	if err != nil {
		t.Fatalf("buildAnswers: %v", err)
	}
	if a.SharedSpace != shared {
		t.Errorf("SharedSpace = %q, want %q", a.SharedSpace, shared)
	}
	if got := a.MemberSpaces["david"]; got != david {
		t.Errorf("MemberSpaces[david] = %q, want %q", got, david)
	}
}

// TestBuildAnswersCarriesTheIdentityAnswer: the flags D7 was about.
//
// setup.Answers has carried Agents, GroupChatID and Persona all along; buildAnswers set
// none of them and no flag existed to, so `--non-interactive` could only ever write
// `agents: shared` with the flat English register — the whole third-scope design was
// unreachable from a scripted install, and an isolated household's group pod had no
// chat id to serve.
func TestBuildAnswersCarriesTheIdentityAnswer(t *testing.T) {
	t.Parallel()
	f := minimalFlags()
	f.mode = "isolated"
	f.agents = "per_member"
	f.groupChatID = -1001234567890
	f.persona = config.PersonaConfig{Language: "Spanish", Tone: "warm", Character: "Knows the house."}

	a, err := buildAnswers(f)
	if err != nil {
		t.Fatalf("buildAnswers: %v", err)
	}
	if a.Agents != config.AgentsPerMember {
		t.Errorf("Agents = %q, want %q", a.Agents, config.AgentsPerMember)
	}
	if a.GroupChatID != -1001234567890 {
		t.Errorf("GroupChatID = %d, want the id given", a.GroupChatID)
	}
	if a.Persona != f.persona {
		t.Errorf("Persona = %+v, want %+v", a.Persona, f.persona)
	}
}

// TestBuildAnswersRejectsAnUnknownAgents: the one identity check that belongs here
// rather than in internal/setup, because it is about the flag's spelling rather than
// about the household. Everything else — per_member in simple mode, per_member with no
// group chat — is refused by the scripted path all three front-ends share.
func TestBuildAnswersRejectsAnUnknownAgents(t *testing.T) {
	t.Parallel()
	f := minimalFlags()
	f.agents = "one-each"
	_, err := buildAnswers(f)
	if err == nil {
		t.Fatal("--agents one-each was accepted")
	}
	if !strings.Contains(err.Error(), "per_member") || !strings.Contains(err.Error(), "shared") {
		t.Errorf("the refusal does not name the two answers: %v", err)
	}
}

// TestScriptedInstallHasNoSecretFlags: a deliberate absence, guarded so nobody adds one
// without meaning to. A secret in argv is a secret in `ps`, in the shell history and in
// the CI log; the configuration names an environment variable for each one, and a script
// exporting those is the channel every deployment path already uses.
func TestScriptedInstallHasNoSecretFlags(t *testing.T) {
	t.Parallel()
	h := newHarness(t, simpleYAML, fullEnvironment())
	h.run("setup", "-h")
	usage := h.both()
	for _, forbidden := range []string{"-bot-token ", "-member-bot-token", "-passphrase", "-api-key "} {
		if strings.Contains(usage, forbidden) {
			t.Errorf("`kenward setup` grew a flag taking a secret on the command line: %q\n%s", forbidden, usage)
		}
	}
	// And the flags this defect was about are there.
	for _, want := range []string{"-agents", "-group-chat-id", "-persona-language", "-persona-tone", "-persona-character"} {
		if !strings.Contains(usage, want) {
			t.Errorf("`kenward setup` has no %s flag:\n%s", want, usage)
		}
	}
}

// TestBuildAnswersRejectsAMalformedMemberSpace: the error names the shape and the way
// out, because the operator reaching for --non-interactive is scripting an install and
// would otherwise put a display name there. The way out is usually to drop the flag —
// setup makes the spaces itself — so the message says that rather than sending them to
// `lore spaces` for an id column they no longer need.
func TestBuildAnswersRejectsAMalformedMemberSpace(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{"david", "=abc", "david="} {
		f := minimalFlags()
		f.sharedSpace = "s"
		f.memberSpaces = stringList{spec}
		_, err := buildAnswers(f)
		if err == nil {
			t.Fatalf("--member-space %q was accepted", spec)
		}
		if !strings.Contains(err.Error(), "ID=SPACE_ID") || !strings.Contains(err.Error(), "omit it") {
			t.Errorf("--member-space %q: error does not say the shape and the way out: %v", spec, err)
		}
	}
}
