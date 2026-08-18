package setup

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BlueHeisenberg/kenward/internal/config"
)

// askMode asks the trust question and turns the answer into a mode.
//
// There is no default. Every other question here suggests one, because a suggestion
// is a kindness; this one must be answered, because it is the only question whose
// wrong answer cannot be undone by editing the file afterwards.
func (w *Wizard) askMode(ctx context.Context) error {
	w.blank()
	choice, err := w.io.AskChoice(TrustQuestion, []string{trustAnswerSimple, trustAnswerIsolated}, -1)
	if err != nil {
		return err
	}
	if choice == 0 {
		w.mode = config.ModeSimple
		return nil
	}
	if w.goos == "linux" {
		w.mode = config.ModeIsolated
		return nil
	}

	w.blank()
	w.io.Print(isolatedNeedsLinux(w.goos))
	w.blank()
	// The default is to stop. Somebody who has just said they want their household
	// sealed should have to say so again before being given the mode that is not,
	// and a stray Enter must never be the thing that downgrades it.
	next, err := w.io.AskChoice("What would you like to do?",
		[]string{isolatedFallbackSimple, isolatedFallbackStop}, 1)
	if err != nil {
		return err
	}
	if next == 1 {
		w.io.Print(stoppedForLinux)
		return ErrStopped
	}
	w.mode = config.ModeSimple
	return nil
}

// askHousehold names the group and its shared memory.
func (w *Wizard) askHousehold(ctx context.Context) error {
	w.blank()
	w.io.Print(householdIntro)
	w.blank()

	name, err := w.io.Ask(questionHouseholdName, DefaultHouseholdName)
	if err != nil {
		return err
	}
	w.household.Name = strings.TrimSpace(name)

	w.blank()
	w.io.Print(sharedSpaceNote)

	id, err := w.makeSpace(ctx, sharedSpaceLabel(w.household.Name), "the household's shared memory")
	if err != nil {
		return err
	}
	w.household.SharedSpace = id
	return nil
}

// sharedSpaceLabel and privateSpaceLabel are the display names the spaces are made
// with. They are prefixed with the household's name so that a lore store shared with
// a person's own use does not end up with a bare "household" in it, and they are the
// same two labels the dashboard's first-run wizard uses, so a household set up either
// way reads the same in `lore spaces`.
func sharedSpaceLabel(household string) string { return household + " — household" }

// privateSpaceLabelFor is one member's, and it is unique within the household.
//
// Two people can be called David. Their member ids are already made unique, but the
// space is named after the person rather than the slug, so the label needs the same
// treatment: without it the second David's space is created with a name lore already
// holds, the ErrSpaceExists path finds the first David's space, and the two share a
// private memory — which is validation's last-line refusal and would be a wizard
// writing exactly the configuration it exists to prevent.
func (w *Wizard) privateSpaceLabelFor(m config.MemberConfig) string {
	label := w.household.Name + " — " + m.Name
	if w.spaceLabels[label] {
		label = w.household.Name + " — " + m.Name + " (" + m.ID + ")"
	}
	if w.spaceLabels == nil {
		w.spaceLabels = map[string]bool{}
	}
	w.spaceLabels[label] = true
	return label
}

// askTelegram takes the household's bot token, explains that it is not stored, and
// offers to put it somewhere it will be read from.
func (w *Wizard) askTelegram(ctx context.Context) error {
	w.blank()
	if w.mode == config.ModeIsolated {
		w.io.Print(telegramIntroIsolated)
	} else {
		w.io.Print(telegramIntroSimple)
	}
	w.blank()
	w.io.Print(botFatherWalkthrough)
	w.blank()

	token, err := w.askToken()
	if err != nil {
		return err
	}

	// The variable is named in both modes. In simple mode kenward requires it; in
	// isolated mode it is the group chat's own bot, and each member's token lives
	// in a variable of their own.
	w.telegram.BotTokenEnv = DefaultBotTokenEnv
	note := "export it yourself before starting kenward"
	if token == "" {
		note = "you said you would set this one yourself"
	}
	w.recordEnv(DefaultBotTokenEnv, "telegram.bot_token_env", note, token)

	w.blank()
	w.io.Print(tokenNotStored(DefaultBotTokenEnv))

	if token == "" {
		return nil
	}

	// Asked here because this is the only moment it can be asked: the token is in
	// hand, the bot is not in any group yet, and Telegram applies a privacy-mode
	// change only to groups the bot joins afterwards. Later means removing the bot
	// from the group and adding it again.
	if err := w.checkBotPrivacy(ctx, token); err != nil {
		return err
	}
	w.blank()
	w.io.Print(envFileNote)
	w.blank()
	want, err := w.io.AskYesNo(questionWriteEnvFile, true)
	if err != nil {
		return err
	}
	w.wantEnvFile = want
	return nil
}

// checkBotPrivacy asks Telegram whether this bot can hear a group chat, and offers to
// ask again once it cannot.
//
// The loop is the point. Printing the instruction and moving on would leave the operator
// with no way to know the fix took, and this is a setting whose failure has no symptom —
// a bot with privacy mode on receives nothing in a group, so there is no error, no log
// line and nothing to search for. Asking again turns a paragraph into something that can
// be verified while they are still standing here.
//
// It never blocks. A household that will never use a group chat is a real household, and
// so is one whose connection is down; both are offered the way out and neither is stopped.
func (w *Wizard) checkBotPrivacy(ctx context.Context, token string) error {
	for {
		info, err := w.botProbe(ctx, token)
		w.blank()
		if err != nil {
			w.io.Print(privacyModeUnknown)
			return nil
		}
		w.io.Print(botIs(info.Username))
		if info.ReadsGroupMessages {
			return nil
		}
		w.blank()
		w.io.Print(privacyModeOn(info.Username))
		w.blank()
		again, err := w.io.AskYesNo(questionCheckPrivacyAgain, true)
		if err != nil {
			return err
		}
		if !again {
			return nil
		}
	}
}

// askToken reads a bot token without echo, checking its shape.
//
// The check is a question rather than a rule. Telegram's token format is theirs to
// change, and a wizard that refused a valid token because it had opinions about it
// would be unfixable from the outside; a wizard that says "that does not look like
// one" catches the pasted username, the truncated copy and the stray quote, which is
// what actually goes wrong.
func (w *Wizard) askToken() (string, error) {
	for {
		token, err := w.io.AskSecret(questionBotToken)
		if err != nil {
			return "", err
		}
		switch {
		case token == "":
			leave, err := w.io.AskYesNo(questionLeaveTokenUnset, false)
			if err != nil {
				return "", err
			}
			if leave {
				return "", nil
			}
		case looksLikeBotToken(token):
			return token, nil
		default:
			w.io.Print(tokenLooksWrong)
			anyway, err := w.io.AskYesNo(questionUseTokenAnyway, false)
			if err != nil {
				return "", err
			}
			if anyway {
				return token, nil
			}
		}
	}
}

// looksLikeBotToken reports whether a string has the shape BotFather hands out: a
// numeric bot id, a colon, and a run of URL-safe characters.
func looksLikeBotToken(s string) bool {
	id, secret, ok := strings.Cut(s, ":")
	if !ok || id == "" || len(secret) < 20 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range secret {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// askMembers collects names and derives everything else from them.
func (w *Wizard) askMembers(ctx context.Context) error {
	w.blank()
	w.io.Print(membersIntro)
	w.blank()

	taken := map[string]bool{}
	for {
		name, err := w.io.Ask(questionMemberName, "")
		if err != nil {
			return err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			if len(w.members) > 0 {
				break
			}
			w.io.Print("  At least one person, or there is nobody for kenward to talk to.")
			continue
		}
		w.addMember(name, taken)
	}

	// The spaces are made after every name is in, so that a household abandoned
	// half way through the list leaves no spaces behind for the people who were
	// never entered.
	for i := range w.members {
		m := &w.members[i]
		id, err := w.makeSpace(ctx,
			w.privateSpaceLabelFor(*m),
			m.Name+"'s private memory")
		if err != nil {
			return err
		}
		m.PrivateSpace = id
	}

	w.blank()
	w.io.Print(memberSummary(w.members))
	if w.mode == config.ModeIsolated {
		w.blank()
		w.io.Print(memberTokenNote(w.members))
	}
	return nil
}

// addMember derives a member's id and — in isolated mode — their own bot token
// variable, from their name. The private space is not derived: it is a lore space,
// made once every name is in, and its id is whatever lore mints.
func (w *Wizard) addMember(name string, taken map[string]bool) {
	id := uniqueSlug(name, taken, fmt.Sprintf("member-%d", len(w.members)+1))
	m := config.MemberConfig{ID: id, Name: name}
	if w.mode == config.ModeIsolated {
		m.BotTokenEnv = envVarFor(MemberBotTokenPrefix, id)
		w.recordEnv(m.BotTokenEnv, fmt.Sprintf("members[%d].bot_token_env", len(w.members)),
			fmt.Sprintf("created when %s enrols", name), "")
		m.PassphraseEnv = envVarFor(MemberPassphrasePrefix, id)
		w.recordEnv(m.PassphraseEnv, fmt.Sprintf("members[%d].passphrase_env", len(w.members)),
			fmt.Sprintf("chosen by whoever runs %s's pod; it wraps %s's key and nobody else's", name, name), "")
	}
	w.members = append(w.members, m)
}

// askIdentity asks how many assistants the household has, and then what kenward's own
// is like.
//
// It comes after the people are named, so that "one each" is a concrete offer about
// people the reader has just listed rather than an abstraction. It comes before the
// endpoints and the tier chains because those are about machines, and this is the last
// question about the household itself.
//
// It says nothing about who can read what. That is the whole discipline of this step:
// it is a presentation question, it is not the trust question, and a wizard that
// implied otherwise would teach the household the misunderstanding internal/privacy
// then has to correct.
//
// It does have to say the one thing the two questions really do share, and only that
// one: one agent each needs a Telegram bot for each member, two agents behind one
// contact are one agent, and only isolated mode gives each member their own. So in
// simple mode the offer is made and then declined with the reason rather than hidden,
// because a household that wants it should learn what it costs rather than never learn
// it exists. config.Config.validateHousehold refuses the same combination in a file.
func (w *Wizard) askIdentity(ctx context.Context) error {
	w.blank()
	w.io.Print(identityIntro)
	w.blank()

	// The default is one assistant. It is today's behaviour, it is what a household
	// that has not thought about this wants, and a stray Enter must give it.
	w.agents = config.AgentsShared
	for {
		choice, err := w.io.AskChoice(IdentityQuestion,
			[]string{identityAnswerShared, identityAnswerPerMember}, 0)
		if err != nil {
			return err
		}
		if choice != 1 {
			break
		}
		if w.mode == config.ModeIsolated {
			w.agents = config.AgentsPerMember
			break
		}
		w.blank()
		w.io.Print(identityNeedsIsolated)
		w.blank()
	}

	// One each puts kenward in the group chat and nowhere else, so the group's id is
	// asked for here and refused blank. It is the same refusal per_member + simple
	// gets, for the same reason: an operator told they have one assistant each must
	// not be handed a household that cannot answer.
	if w.agents == config.AgentsPerMember {
		id, err := w.askGroupChat()
		if err != nil {
			return err
		}
		w.household.GroupChatID = id
	}

	w.blank()
	if w.agents == config.AgentsPerMember {
		w.io.Print(personaIntroPerMember)
	} else {
		w.io.Print(personaIntroShared)
	}

	for _, q := range []struct {
		question string
		note     string
		what     string
		limit    int
		field    *string
	}{
		{questionPersonaLanguage, personaLanguageNote, "a language", config.MaxPersonaLine, &w.persona.Language},
		{questionPersonaTone, personaToneNote, "a register", config.MaxPersonaLine, &w.persona.Tone},
		{questionPersonaCharacter, personaCharacterNote, "a character", config.MaxPersonaCharacter, &w.persona.Character},
	} {
		w.blank()
		w.io.Print(q.note)
		w.blank()
		for {
			answer, err := w.io.Ask(q.question, "")
			if err != nil {
				return err
			}
			answer = strings.TrimSpace(answer)
			if utf8.RuneCountInString(answer) > q.limit {
				w.io.Print(personaTooLong(q.what, q.limit))
				continue
			}
			*q.field = answer
			break
		}
	}
	return nil
}

// askGroupChat takes the household group's Telegram chat id, and will not take a blank
// one.
//
// It re-asks rather than accepting nothing, because nothing is the answer that produces
// the household this question exists to prevent. Any non-zero whole number is accepted:
// the shape check is a question rather than a rule, exactly like the bot token's, since
// Telegram's numbering is theirs to change and a wizard with opinions about it would be
// unfixable from the outside.
func (w *Wizard) askGroupChat() (int64, error) {
	w.blank()
	w.io.Print(groupChatIntro)
	w.blank()
	for {
		answer, err := w.io.Ask(questionGroupChatID, "")
		if err != nil {
			return 0, err
		}
		if id, ok := parseGroupChatID(answer); ok {
			return id, nil
		}
		w.io.Print(badGroupChatID(answer))
	}
}

// parseGroupChatID reads a chat id. Zero is not one: it is the value that means "no
// group is configured", which is what this question refuses to write.
func parseGroupChatID(answer string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(answer), 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// askEndpoints collects the machines that run the model, probing each one as it is
// entered.
func (w *Wizard) askEndpoints(ctx context.Context) error {
	w.blank()
	w.io.Print(endpointsIntro)

	names := map[string]bool{}
	for {
		if err := w.askOneEndpoint(ctx, names); err != nil {
			return err
		}
		w.blank()
		another, err := w.io.AskYesNo(questionAnotherEndpoint, false)
		if err != nil {
			return err
		}
		if !another {
			return nil
		}
	}
}

// askOneEndpoint collects one endpoint and probes it.
func (w *Wizard) askOneEndpoint(ctx context.Context, names map[string]bool) error {
	w.blank()
	name, err := w.askEndpointName(names)
	if err != nil {
		return err
	}

	baseURL, err := w.askBaseURL(ctx)
	if err != nil {
		return err
	}

	var model string
	for {
		answer, err := w.io.Ask(questionEndpointModel, "")
		if err != nil {
			return err
		}
		model = strings.TrimSpace(answer)
		if model != "" {
			break
		}
		w.io.Print("  The name the server itself uses, exactly as it reports it.")
	}

	endpoint := config.EndpointConfig{Name: name, BaseURL: baseURL, Model: model}

	local := isLocal(baseURL)
	needsKey, err := w.io.AskYesNo(questionEndpointKey, !local)
	if err != nil {
		return err
	}
	if needsKey {
		keyEnv, err := w.io.Ask(questionEndpointKeyEnv, apiKeyEnvFor(name))
		if err != nil {
			return err
		}
		endpoint.APIKeyEnv = strings.TrimSpace(keyEnv)
		key, err := w.io.AskSecret(questionEndpointKeyVal)
		if err != nil {
			return err
		}
		note := "export it yourself before starting kenward"
		if key != "" {
			note = "add it to your environment"
		}
		w.recordEnv(endpoint.APIKeyEnv, fmt.Sprintf("endpoints[%d].api_key_env", len(w.endpoints)), note, key)
	}

	w.blank()
	w.io.Print(endpointTiersNote)
	w.blank()
	defaultTier := LocalTier
	if !local {
		defaultTier = CloudTier
	}
	for {
		answer, err := w.io.Ask(questionEndpointTiers, defaultTier)
		if err != nil {
			return err
		}
		if tags := parseTiers(answer); len(tags) > 0 {
			endpoint.Tags = tags
			break
		}
		w.io.Print("  At least one tier, or nothing can ever route to it.")
	}

	w.endpoints = append(w.endpoints, endpoint)
	return nil
}

// askEndpointName takes a label, refusing a blank one and a repeat.
func (w *Wizard) askEndpointName(names map[string]bool) (string, error) {
	for {
		answer, err := w.io.Ask(questionEndpointName, "")
		if err != nil {
			return "", err
		}
		name := strings.TrimSpace(answer)
		switch {
		case name == "":
			w.io.Print("  It needs a label — it is how refusals and cooldowns name it later.")
		case names[name]:
			w.io.Print(fmt.Sprintf("  There is already an endpoint called %q.", name))
		default:
			names[name] = true
			return name, nil
		}
	}
}

// askBaseURL takes an address and connects to it, so a typo is found while the
// person who made it is still looking at the line they typed.
//
// An address that cannot be dialled at all is the one probe result that sends the
// question round again: everything else — refused, unresolved, silent — is a fact
// about a machine rather than a mistake in the answer, and is recorded.
func (w *Wizard) askBaseURL(ctx context.Context) (string, error) {
	for {
		answer, err := w.io.Ask(questionEndpointBaseURL, "")
		if err != nil {
			return "", err
		}
		raw := strings.TrimSpace(answer)
		if raw == "" {
			w.io.Print("  An address, like http://monster.tail:8000/v1.")
			continue
		}
		result := w.probe(ctx, raw)
		if result.State == BadURL {
			w.io.Print(fmt.Sprintf("  %v", result.Err))
			continue
		}
		w.io.Print(result.describe())
		return raw, nil
	}
}

// parseTiers splits a comma-separated answer into tier names.
func parseTiers(answer string) []string {
	var tiers []string
	seen := map[string]bool{}
	for _, part := range strings.Split(answer, ",") {
		tier := Slugify(part)
		if tier == "" || seen[tier] {
			continue
		}
		seen[tier] = true
		tiers = append(tiers, tier)
	}
	return tiers
}

// tierPlan is what the collected endpoints allow, split by whether a tier can be
// served by a machine outside the household.
type tierPlan struct {
	// local are tiers every endpoint of which is on the household's own network.
	local []string
	// cloud are the rest: a tier with even one endpoint outside the house is one
	// that can leave it.
	cloud []string
	// cloudHosts names the hosts a cloud tier can reach, for the opt-in question.
	// Somebody deciding whether to allow it should be told where it goes.
	cloudHosts []string
}

func (w *Wizard) tierPlan() tierPlan {
	var order []string
	leaves := map[string]bool{}
	hosts := map[string][]string{}
	for _, e := range w.endpoints {
		for _, tag := range e.Tags {
			if _, seen := hosts[tag]; !seen {
				order = append(order, tag)
				hosts[tag] = nil
			}
			if !isLocal(e.BaseURL) {
				leaves[tag] = true
				hosts[tag] = append(hosts[tag], hostOf(e.BaseURL))
			}
		}
	}

	var plan tierPlan
	seenHost := map[string]bool{}
	for _, tag := range order {
		if !leaves[tag] {
			plan.local = append(plan.local, tag)
			continue
		}
		plan.cloud = append(plan.cloud, tag)
		for _, h := range hosts[tag] {
			if h != "" && !seenHost[h] {
				seenHost[h] = true
				plan.cloudHosts = append(plan.cloudHosts, h)
			}
		}
	}
	return plan
}

// askTiers decides where each conversation may go.
//
// The default is local-only for every private space and for the household's shared
// one, and adding a tier that leaves the house is a separate question with a
// separate yes. That asymmetry is the whole point: the privacy-preserving answer
// costs nothing, and the other one has to be typed.
func (w *Wizard) askTiers(ctx context.Context) error {
	plan := w.tierPlan()
	w.blank()
	w.io.Print(tiersIntro)

	if len(plan.local) == 0 {
		w.blank()
		w.io.Print(tiersNoLocalWarning)
		w.blank()
		ok, err := w.io.AskYesNo(
			fmt.Sprintf("Use %s for private conversations anyway?", formatChain(plan.cloud)), false)
		if err != nil {
			return err
		}
		if !ok {
			w.io.Print(stoppedNoLocal)
			return ErrStopped
		}
		for i := range w.members {
			w.members[i].Tiers = append([]string(nil), plan.cloud...)
		}
		w.household.Tiers = append([]string(nil), plan.cloud...)
		return nil
	}

	for i := range w.members {
		m := &w.members[i]
		w.blank()
		w.io.Print(privateDefaultNote(m.Name, plan.local))
		chain := append([]string(nil), plan.local...)
		if len(plan.cloud) > 0 {
			w.blank()
			w.io.Print(cloudConsequence(m.Name+"'s private messages", plan.cloud, plan.cloudHosts))
			w.blank()
			yes, err := w.io.AskYesNo(cloudOptIn(m.Name, plan.cloud), false)
			if err != nil {
				return err
			}
			if yes {
				chain = append(chain, plan.cloud...)
			}
		}
		m.Tiers = chain
	}

	w.blank()
	w.io.Print(groupDefaultNote(plan.local))
	chain := append([]string(nil), plan.local...)
	if len(plan.cloud) > 0 {
		w.blank()
		w.io.Print(cloudConsequence("group messages", plan.cloud, plan.cloudHosts))
		w.blank()
		yes, err := w.io.AskYesNo(groupCloudOptIn(plan.cloud), false)
		if err != nil {
			return err
		}
		if yes {
			chain = append(chain, plan.cloud...)
		}
	}
	w.household.Tiers = chain
	return nil
}

// askHistory asks how often a conversation's recent messages are dropped.
//
// It is the last question, and it is the only one here that is not about the
// household or its machines. It earns the slot anyway: it is the one setting whose
// symptom — the assistant losing the thread — is indistinguishable from a fault, so
// somebody who was never shown the question would have no reason to believe the
// behaviour was theirs to choose.
//
// Off is the default and off is what pressing Enter gives, so the wizard's own answer
// is the one that changes nothing about how kenward has always behaved.
func (w *Wizard) askHistory(ctx context.Context) error {
	w.blank()
	w.io.Print(historyIntro)
	for {
		w.blank()
		answer, err := w.io.Ask(historyQuestion, "off")
		if err != nil {
			return err
		}
		d, ok := parseHistoryReset(answer)
		if !ok {
			w.io.Print(badHistoryReset(answer))
			continue
		}
		w.historyReset = config.Duration(d)
		return nil
	}
}

// parseHistoryReset reads the answer to historyQuestion, in either of the two forms
// it may take: a word meaning off, or a duration up to config.MaxHistoryReset.
//
// "0s" is accepted alongside "off" and "none" because it is what the written file
// says, and somebody rerunning setup with the old file open in front of them will
// type what they can see.
func parseHistoryReset(answer string) (time.Duration, bool) {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "off", "no", "none", "never":
		return 0, true
	}
	d, err := time.ParseDuration(strings.TrimSpace(answer))
	if err != nil || d < 0 || d > config.MaxHistoryReset {
		return 0, false
	}
	return d, true
}

// collectScripted fills the wizard in from pre-supplied answers, for
// `--non-interactive`.
//
// It takes the same defaults the questions offer, including the local-only tier
// chains: a scripted install that says nothing about a member's chain gets the
// private one, never the wide one. Endpoints are still probed and the result still
// printed, because an install script's output is read by somebody eventually and a
// machine that did not answer is worth knowing about.
func (w *Wizard) collectScripted(ctx context.Context, a *Answers) error {
	w.mode = a.Mode
	if w.mode == "" {
		w.mode = config.ModeSimple
	}
	if w.mode == config.ModeIsolated && w.goos != "linux" {
		return fmt.Errorf("%w: this is %s", ErrNotLinux, osName(w.goos))
	}

	w.household.Name = orDefault(a.HouseholdName, DefaultHouseholdName)
	w.agents = a.Agents
	if w.agents == "" {
		w.agents = config.DefaultAgents
	}
	w.persona = a.Persona
	w.household.GroupChatID = a.GroupChatID
	// The same refusal the interactive question makes, for the same reason: under one
	// agent each kenward lives only in the group chat, so a household with no group
	// chat id has no kenward in it — not in the group, and not in a private chat. A
	// scripted install must not be able to produce that quietly either.
	if w.agents == config.AgentsPerMember && w.household.GroupChatID == 0 {
		return fmt.Errorf("setup: one assistant each needs household.group_chat_id, the Telegram id of the household's own group; under `agents: per_member` every private chat belongs to somebody's own assistant and the group is the only place kenward speaks, so a household without one cannot be reached at all")
	}

	// A space id is optional here, and the ordinary case is not to give one. An
	// install script that names nothing gets the spaces made for it, which is what
	// lets `kenward setup --non-interactive` be the whole of an install; the
	// dashboard's first-run wizard makes its own and drives this package with the
	// ids, so both are supported and a supplied id is still checked.
	w.household.SharedSpace = strings.TrimSpace(a.SharedSpace)
	if w.household.SharedSpace == "" {
		id, err := w.makeSpace(ctx, sharedSpaceLabel(w.household.Name), "the household's shared memory")
		if err != nil {
			return err
		}
		w.household.SharedSpace = id
	} else if err := w.checkSpaceID(ctx, w.household.SharedSpace, "a household's shared memory"); err != nil {
		return err
	}

	tokenEnv := orDefault(a.BotTokenEnv, DefaultBotTokenEnv)
	w.telegram.BotTokenEnv = tokenEnv
	note := "export it yourself before starting kenward"
	if a.BotToken != "" && a.WriteEnvFile {
		note = "in the .env file just written"
	}
	w.recordEnv(tokenEnv, "telegram.bot_token_env", note, a.BotToken)
	w.wantEnvFile = a.WriteEnvFile

	if len(a.MemberNames) == 0 {
		return fmt.Errorf("setup: at least one member is needed, or there is nobody for kenward to talk to")
	}
	taken := map[string]bool{}
	for _, name := range a.MemberNames {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("setup: a member with no name")
		}
		w.addMember(name, taken)
	}
	for i := range w.members {
		m := &w.members[i]
		space := strings.TrimSpace(a.MemberSpaces[m.ID])
		if space == "" {
			made, err := w.makeSpace(ctx, w.privateSpaceLabelFor(*m), m.Name+"'s private memory")
			if err != nil {
				return err
			}
			space = made
		} else if err := w.checkSpaceID(ctx, space, "a member's private memory"); err != nil {
			return err
		}
		m.PrivateSpace = space

		// Each member's own two secrets, where the caller has them. addMember has
		// already recorded both variables with no value and a note saying where the
		// value will come from; this replaces that note for the ones that arrived, so
		// they reach the .env file and the closing block says so.
		//
		// In simple mode neither variable exists and a value supplied for one is
		// dropped rather than invented into the file: one bot, one node passphrase,
		// and nothing per member to attach it to.
		if w.mode != config.ModeIsolated {
			continue
		}
		envNote := "export it yourself before starting kenward"
		if a.WriteEnvFile {
			envNote = "in the .env file just written"
		}
		if tok := strings.TrimSpace(a.MemberBotTokens[m.ID]); tok != "" {
			w.recordEnv(m.BotTokenEnv, fmt.Sprintf("members[%d].bot_token_env", i), envNote, tok)
		}
		if pass := a.MemberPassphrases[m.ID]; strings.TrimSpace(pass) != "" {
			w.recordEnv(m.PassphraseEnv, fmt.Sprintf("members[%d].passphrase_env", i), envNote, pass)
		}
	}

	if len(a.Endpoints) == 0 {
		return fmt.Errorf("setup: at least one endpoint is needed, or nothing can answer")
	}
	for i, e := range a.Endpoints {
		endpoint := config.EndpointConfig{
			Name:      strings.TrimSpace(e.Name),
			BaseURL:   strings.TrimSpace(e.BaseURL),
			Model:     strings.TrimSpace(e.Model),
			APIKeyEnv: strings.TrimSpace(e.APIKeyEnv),
			Tags:      e.Tiers,
			// Zero leaves ApplyDefaults to fill in the modest default, which
			// is what the terminal wizard always produces.
			ContextWindow: e.ContextWindow,
		}
		if len(endpoint.Tags) == 0 {
			if isLocal(endpoint.BaseURL) {
				endpoint.Tags = []string{LocalTier}
			} else {
				endpoint.Tags = []string{CloudTier}
			}
		}
		if endpoint.APIKeyEnv != "" {
			keyNote := "export it yourself before starting kenward"
			if e.APIKey != "" && a.WriteEnvFile {
				keyNote = "in the .env file just written"
			}
			w.recordEnv(endpoint.APIKeyEnv, fmt.Sprintf("endpoints[%d].api_key_env", i), keyNote, e.APIKey)
		}
		w.io.Print(fmt.Sprintf("%s %s", endpoint.Name, endpoint.BaseURL))
		w.io.Print(w.probe(ctx, endpoint.BaseURL).describe())
		w.endpoints = append(w.endpoints, endpoint)
	}

	plan := w.tierPlan()
	for i := range w.members {
		m := &w.members[i]
		if chain, ok := a.MemberTiers[m.ID]; ok && len(chain) > 0 {
			m.Tiers = append([]string(nil), chain...)
			continue
		}
		m.Tiers = defaultChain(plan)
	}
	// history.reset_every is deliberately absent from Answers, like search_limit and
	// idle_timeout: it is a setting with a safe default, and a scripted install that
	// says nothing about one gets the default. The wizard asks because a person at a
	// terminal can read the question; a script that wants a schedule edits the file
	// it just generated.

	if len(a.GroupTiers) > 0 {
		w.household.Tiers = append([]string(nil), a.GroupTiers...)
	} else {
		w.household.Tiers = defaultChain(plan)
	}
	return nil
}

// defaultChain is the chain a space gets when nobody said otherwise: everything
// that stays in the house, or — when there is nothing in the house — everything,
// since a chain naming no tier at all would refuse every turn without saying why.
func defaultChain(plan tierPlan) []string {
	if len(plan.local) > 0 {
		return append([]string(nil), plan.local...)
	}
	return append([]string(nil), plan.cloud...)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}
