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

	mode := fs.String("mode", string(config.ModeSimple), "simple | isolated")
	household := fs.String("household-name", setup.DefaultHouseholdName, "what this household is called")
	sharedSpace := fs.String("shared-space", setup.DefaultSharedSpace, "the lore space the group chat reads and writes")
	botTokenEnv := fs.String("bot-token-env", setup.DefaultBotTokenEnv, "the variable the household bot's token is read from")
	var members stringList
	fs.Var(&members, "member", "a member's name; repeat for each person (ids are derived)")
	var endpoints stringList
	fs.Var(&endpoints, "endpoint", "name=NAME,url=BASE_URL,model=MODEL[,key-env=VAR][,tiers=a|b]; repeat")
	var memberTiers stringList
	fs.Var(&memberTiers, "member-tiers", "ID=tier|tier — override one member's chain; repeat. Omitted members stay local-only")
	groupTiers := fs.String("group-tiers", "", "tier|tier for the household chain; empty stays local-only")
	writeEnvFile := fs.Bool("write-env-file", false, "write collected secrets to a .env beside the configuration")
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
		answers, err := buildAnswers(*mode, *household, *sharedSpace, *botTokenEnv,
			members, endpoints, memberTiers, *groupTiers, *writeEnvFile)
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

// buildAnswers turns the --non-interactive flags into setup.Answers.
func buildAnswers(mode, household, shared, botTokenEnv string,
	members, endpoints, memberTiers stringList, groupTiers string, writeEnv bool) (*setup.Answers, error) {

	m := config.Mode(mode)
	if m != config.ModeSimple && m != config.ModeIsolated {
		return nil, fmt.Errorf("--mode must be %q or %q, got %q", config.ModeSimple, config.ModeIsolated, mode)
	}
	if len(members) == 0 {
		return nil, errors.New("--non-interactive needs at least one --member NAME")
	}
	if len(endpoints) == 0 {
		return nil, errors.New("--non-interactive needs at least one --endpoint")
	}

	a := &setup.Answers{
		Mode:          m,
		HouseholdName: household,
		SharedSpace:   shared,
		BotTokenEnv:   botTokenEnv,
		MemberNames:   members,
		WriteEnvFile:  writeEnv,
	}
	for _, spec := range endpoints {
		ep, err := parseEndpointSpec(spec)
		if err != nil {
			return nil, err
		}
		a.Endpoints = append(a.Endpoints, ep)
	}
	for _, spec := range memberTiers {
		id, tiers, ok := strings.Cut(spec, "=")
		if !ok || id == "" {
			return nil, fmt.Errorf("--member-tiers wants ID=tier|tier, got %q", spec)
		}
		if a.MemberTiers == nil {
			a.MemberTiers = map[string][]string{}
		}
		a.MemberTiers[id] = splitTiers(tiers)
	}
	if groupTiers != "" {
		a.GroupTiers = splitTiers(groupTiers)
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
