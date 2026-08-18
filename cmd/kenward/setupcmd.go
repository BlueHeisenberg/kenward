package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdSetup(e *env, args []string) int {
	fs := newFlagSet(e, "setup", "kenward setup [--config PATH] [--data-dir PATH] [--force] [--non-interactive ...]")
	configPath := fs.String("config", "", "where to write kenward.yaml (default: ./"+setup.DefaultConfigFileName+")")
	dataDir := fs.String("data-dir", "", "write this data_dir into the configuration")
	force := fs.Bool("force", false, "replace an existing configuration")
	nonInteractive := fs.Bool("non-interactive", false, "ask nothing; take every answer from the flags below")

	var f setupFlags
	fs.StringVar(&f.mode, "mode", string(config.ModeSimple), "simple | isolated")
	fs.StringVar(&f.household, "household-name", setup.DefaultHouseholdName, "what this household is called")
	// No default. A space is the id of something that already exists in lore, so
	// there is nothing to guess: the old default was a display name, and a name
	// configured here writes memory happily and returns nothing on the first read.
	// The backquoted word is the placeholder the flag package prints, so it goes on
	// the value's shape and nowhere else.
	fs.StringVar(&f.sharedSpace, "shared-space", "", "`SPACE_ID` of an existing lore space for the group chat; omit it and setup makes one")
	fs.StringVar(&f.botTokenEnv, "bot-token-env", setup.DefaultBotTokenEnv, "the variable the household bot's token is read from")
	fs.StringVar(&f.agents, "agents", "", "`shared` | per_member — one assistant for the whole household, or one each. per_member needs --mode isolated and --group-chat-id")
	fs.Int64Var(&f.groupChatID, "group-chat-id", 0, "the Telegram id of the household's own group chat; required under --agents per_member")
	fs.StringVar(&f.persona.Language, "persona-language", "", "the language kenward writes in; empty follows whoever is speaking")
	fs.StringVar(&f.persona.Tone, "persona-tone", "", "the register kenward writes in; empty is the flat one")
	fs.StringVar(&f.persona.Character, "persona-character", "", "a line or two of character for kenward; empty is none")
	fs.Var(&f.members, "member", "a member's name; repeat for each person (ids are derived)")
	fs.Var(&f.memberSpaces, "member-space", "`ID=SPACE_ID` of an existing lore space for that member's private memory; omit it and setup makes one")
	fs.Var(&f.endpoints, "endpoint", "name=NAME,url=BASE_URL,model=MODEL[,key-env=VAR][,tiers=a|b]; repeat")
	fs.Var(&f.memberTiers, "member-tiers", "ID=tier|tier — override one member's chain; repeat. Omitted members stay local-only")
	fs.StringVar(&f.groupTiers, "group-tiers", "", "tier|tier for the household chain; empty stays local-only")
	fs.BoolVar(&f.writeEnvFile, "write-env-file", false, "write collected secrets to a .env beside the configuration")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		e.errorf("setup takes no positional arguments; got %q", fs.Arg(0))
		return exitUsage
	}

	opts := setup.Options{
		ConfigPath: *configPath,
		DataDir:    resolveDataDir(e, *dataDir),
		Force:      *force,
		GOOS:       e.os(),
		LookupEnv:  e.env(),
	}
	if *nonInteractive {
		answers, err := buildAnswers(&f)
		if err != nil {
			e.errorf("%v", err)
			return exitUsage
		}
		opts.Answers = answers
	}

	w := setup.New(setup.NewConsoleIO(e.stdin, e.stdout), opts)
	_, err := w.Run(e.context())
	switch {
	case errors.Is(err, setup.ErrStopped), errors.Is(err, setup.ErrInputClosed):
		// Somebody changed their mind, or pressed Ctrl-D. Nothing was written.
		// Reporting a failure at them would be rude and wrong.
		return exitOK
	case errors.Is(err, setup.ErrExists):
		e.errorf("%v\n\nA household's configuration is full of decisions somebody made once. Pass\n"+
			"--force if replacing it is what you meant.", err)
		return exitUsage
	case errors.Is(err, setup.ErrNotLinux):
		e.errorf("%v\n\nA script that asked for isolated mode is never quietly given simple mode.", err)
		return exitUsage
	case err != nil:
		e.errorf("%v", err)
		return exitFailure
	}

	// The wizard has already told the operator what it wrote, which variables have
	// to exist and what to run next. Repeating any of that here would be a second
	// copy of the same closing screen, and the wizard's is the one whose wording is
	// tested. The only thing left to say is the one thing it cannot know: where the
	// secrets it collected ended up, when it was asked to write them.
	if p := w.EnvFilePath(); p != "" {
		e.printf("\n%s holds the secrets you typed. It is already covered by .gitignore;\n", p)
		e.printf("keep it that way, and keep its mode at 0600.\n")
	}
	return exitOK
}

// setupFlags is every `--non-interactive` answer.
//
// Gathered into a struct rather than passed one argument at a time: the list is long
// enough that positional parameters stop being readable, and a flag whose value never
// reaches setup.Answers is the whole of defect D7.
//
// There is deliberately no flag for any secret — no bot token, no member passphrase,
// no endpoint API key. A value in argv is a value in `ps`, in the shell history and in
// the CI log, and the configuration already names an environment variable for each one:
// the script exports `KENWARD_BOT_TOKEN` and the per-member variables the file names,
// which is the channel every deployment path already uses. The dashboard's wizard has
// fields for them because a browser form has no other channel; a script does.
// `--write-env-file` therefore writes an .env holding only what setup collected, which
// under `--non-interactive` is nothing.
type setupFlags struct {
	mode         string
	household    string
	sharedSpace  string
	botTokenEnv  string
	agents       string
	groupChatID  int64
	persona      config.PersonaConfig
	members      stringList
	memberSpaces stringList
	endpoints    stringList
	memberTiers  stringList
	groupTiers   string
	writeEnvFile bool
}

// buildAnswers turns the --non-interactive flags into setup.Answers.
//
// Spaces are passed through rather than checked here. internal/setup validates them
// against lore's real listing and refuses a display name or a personal space by
// message, and a scripted install must not be able to write a configuration the
// interactive wizard would have refused — which is exactly what a second, weaker check
// in this package would allow. The same reasoning keeps the identity combinations out
// of here: `per_member` in simple mode, and `per_member` with no group chat id, are
// both refused by internal/setup's scripted path, which is the one place the terminal
// wizard, the dashboard and this command all pass through.
func buildAnswers(f *setupFlags) (*setup.Answers, error) {
	m := config.Mode(f.mode)
	if m != config.ModeSimple && m != config.ModeIsolated {
		return nil, fmt.Errorf("--mode must be %q or %q, got %q", config.ModeSimple, config.ModeIsolated, f.mode)
	}
	agents := config.Agents(strings.TrimSpace(f.agents))
	switch agents {
	case "", config.AgentsShared, config.AgentsPerMember:
	default:
		return nil, fmt.Errorf("--agents must be %q or %q, got %q", config.AgentsShared, config.AgentsPerMember, f.agents)
	}
	if len(f.members) == 0 {
		return nil, errors.New("--non-interactive needs at least one --member NAME")
	}
	if len(f.endpoints) == 0 {
		return nil, errors.New("--non-interactive needs at least one --endpoint")
	}

	a := &setup.Answers{
		Mode:          m,
		HouseholdName: f.household,
		SharedSpace:   f.sharedSpace,
		BotTokenEnv:   f.botTokenEnv,
		Agents:        agents,
		GroupChatID:   f.groupChatID,
		Persona:       f.persona,
		MemberNames:   f.members,
		WriteEnvFile:  f.writeEnvFile,
	}
	for _, spec := range f.endpoints {
		ep, err := parseEndpointSpec(spec)
		if err != nil {
			return nil, err
		}
		a.Endpoints = append(a.Endpoints, ep)
	}
	for _, spec := range f.memberSpaces {
		id, space, ok := strings.Cut(spec, "=")
		if !ok || id == "" || strings.TrimSpace(space) == "" {
			return nil, fmt.Errorf("--member-space wants ID=SPACE_ID, got %q; omit it entirely and setup makes the space itself", spec)
		}
		if a.MemberSpaces == nil {
			a.MemberSpaces = map[string]string{}
		}
		a.MemberSpaces[id] = strings.TrimSpace(space)
	}
	for _, spec := range f.memberTiers {
		id, tiers, ok := strings.Cut(spec, "=")
		if !ok || id == "" {
			return nil, fmt.Errorf("--member-tiers wants ID=tier|tier, got %q", spec)
		}
		if a.MemberTiers == nil {
			a.MemberTiers = map[string][]string{}
		}
		a.MemberTiers[id] = splitTiers(tiers)
	}
	if f.groupTiers != "" {
		a.GroupTiers = splitTiers(f.groupTiers)
	}
	return a, nil
}

func parseEndpointSpec(spec string) (setup.EndpointAnswer, error) {
	var ep setup.EndpointAnswer
	for _, field := range strings.Split(spec, ",") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return ep, fmt.Errorf("--endpoint field %q wants key=value", field)
		}
		switch strings.TrimSpace(key) {
		case "name":
			ep.Name = value
		case "url":
			ep.BaseURL = value
		case "model":
			ep.Model = value
		case "key-env":
			ep.APIKeyEnv = value
		case "tiers":
			ep.Tiers = splitTiers(value)
		default:
			return ep, fmt.Errorf("--endpoint has no field %q", key)
		}
	}
	if ep.Name == "" || ep.BaseURL == "" || ep.Model == "" {
		return ep, fmt.Errorf("--endpoint %q needs at least name=, url= and model=", spec)
	}
	return ep, nil
}

// splitTiers accepts "a|b" rather than "a,b" because commas already separate the
// fields of an --endpoint spec.
func splitTiers(s string) []string {
	var out []string
	for _, t := range strings.Split(s, "|") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
