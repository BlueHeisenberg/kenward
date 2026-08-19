package dashboard

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/enrol"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// ---------------------------------------------------------------- first run

type setupTokenData struct {
	Outstanding bool
}

func (s *Server) handleSetupToken(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "setup_token", page{
		Title: "First run",
		Data:  setupTokenData{Outstanding: s.tokens.Outstanding(s.deps.now())},
		Error: r.URL.Query().Get("err"),
	})
}

// handleSetupTokenSubmit exchanges the printed token for a session.
//
// The token is destroyed inside Redeem, before this returns, so the exchange is the only
// thing it can ever do. What comes back is an ordinary session cookie with a fresh id —
// nothing adopts the token as an identifier, and nothing promotes a session in place.
func (s *Server) handleSetupTokenSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if err := s.tokens.Redeem(token, s.deps.now()); err != nil {
		// Logged, because a wrong token on a socket that is supposed to be loopback
		// is worth seeing, and shown, because the ordinary cause is a paste that
		// picked up a newline.
		s.deps.logger().Warn("dashboard", "event", "setup_token_rejected")
		s.render(w, http.StatusForbidden, "setup_token", page{
			Title: "First run",
			Data:  setupTokenData{Outstanding: s.tokens.Outstanding(s.deps.now())},
			Error: err.Error(),
		})
		return
	}

	sess, err := s.sess.create(kindSetup)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sess.wizard = newWizardState()
	setCookie(w, setupCookieName, sess.id, s.secure())
	http.Redirect(w, r, "/setup/"+wizardSteps[0].Slug, http.StatusSeeOther)
}

// withWizardSession accepts either kind of session for the wizard's own steps.
//
// Before the admin account exists there is only the setup session, which the token
// bought. From the moment the account exists there is only an admin session, because the
// account step destroys every setup session in the same breath as creating the account.
// So this is not two doors: it is one flow whose credential is upgraded halfway through,
// and the upgrade is what makes the rest of the questions ones a stranger cannot answer.
func (s *Server) withWizardSession(next func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sess, ok := s.sessionFrom(r, adminCookieName, kindAdmin); ok {
			next(w, r, sess)
			return
		}
		if s.admin.Exists() {
			// The account exists, so a setup session cannot: sign in.
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		sess, ok := s.sessionFrom(r, setupCookieName, kindSetup)
		if !ok {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
			http.Error(w, "no setup session", http.StatusUnauthorized)
			return
		}
		next(w, r, sess)
	}
}

type wizardData struct {
	Steps   []wizardStep
	Current int
	Slug    string
	State   *wizardState
	// ConfigPath is where the wizard will write, named on the review step so nobody
	// finds out afterwards which file they just created.
	ConfigPath string
	// ExistingConfig is set when a kenward.yaml is already there, which turns the
	// wizard into an account-creation flow and nothing else.
	ExistingConfig string
	// Statement is the privacy claim for the mode currently chosen, shown on the
	// trust step and again on review. It comes from internal/privacy verbatim.
	Statement string
	// Back is the slug of the step the Back control returns to, or empty on the first
	// one. Without it the only way back through this wizard was editing the URL, which
	// made every refusal that says "go back and choose the other thing" unfollowable.
	Back string
	// Problems are the validation faults the review step found.
	Problems []string
	// MinPassword is what the account step asks for, stated in the label rather than
	// discovered by having a password refused.
	MinPassword int
}

func (s *Server) handleWizardStep(w http.ResponseWriter, r *http.Request, sess *session) {
	slug := r.PathValue("step")
	idx := stepIndex(slug)
	if idx < 0 {
		s.notFound(w, r)
		return
	}
	if sess.wizard == nil {
		sess.wizard = newWizardState()
	}
	// Steps past the account cannot be reached before it exists. Ordering the
	// questions is not enough on its own — a URL is a way of skipping to one.
	if idx > stepIndex("admin") && !s.admin.Exists() {
		http.Redirect(w, r, "/setup/admin", http.StatusSeeOther)
		return
	}
	// A step this household does not have is walked past rather than 404ed: it is a
	// step of this wizard, it is simply not one of this household's questions, and
	// the flow arrives here by following its own Continue.
	if !wizardSteps[idx].applies(sess.wizard) {
		http.Redirect(w, r, "/setup/"+nextStep(idx, sess.wizard), http.StatusSeeOther)
		return
	}
	s.renderWizard(w, http.StatusOK, sess, idx, page{Flash: flashOf(r)})
}

func (s *Server) renderWizard(w http.ResponseWriter, status int, sess *session, idx int, p page, problems ...string) {
	st := sess.wizard
	existing := ""
	if _, err := os.Stat(s.deps.ConfigPath); err == nil {
		existing = s.deps.ConfigPath
	}
	p.Title = wizardSteps[idx].Title
	p.CSRF = sess.csrf
	p.SignedIn = sess.kind == kindAdmin
	p.Nav = "setup"
	if wizardSteps[idx].Slug == "bots" {
		// Rebuilt from the household as it stands now, so that going Back and adding
		// somebody does not leave this page asking about the old one.
		st.syncMemberSecrets()
	}
	// The progress list shows the steps this household actually has, so the number of
	// them does not change under somebody who chose simple mode.
	var shown []wizardStep
	current := 0
	for i, step := range wizardSteps {
		if !step.applies(st) {
			continue
		}
		if i == idx {
			current = len(shown)
		}
		shown = append(shown, step)
	}
	p.Data = wizardData{
		Steps:          shown,
		Current:        current,
		Slug:           wizardSteps[idx].Slug,
		Back:           prevStep(idx, st),
		State:          st,
		ConfigPath:     s.deps.ConfigPath,
		ExistingConfig: existing,
		Statement:      privacy.Statement(privacyModeFor(st.Mode)),
		Problems:       problems,
		MinPassword:    MinPasswordLength,
	}
	s.render(w, status, "wizard", p)
}

func (s *Server) handleWizardSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	slug := r.PathValue("step")
	idx := stepIndex(slug)
	if idx < 0 {
		s.notFound(w, r)
		return
	}
	if sess.wizard == nil {
		sess.wizard = newWizardState()
	}
	if idx > stepIndex("admin") && !s.admin.Exists() {
		http.Error(w, "there is no admin account yet", http.StatusForbidden)
		return
	}
	// A step this household does not have has no answers to record. Walked past rather
	// than refused, the same way its GET is: the form is real, it is simply not this
	// household's question, and recording what a stray POST said would leave the review
	// screen restating per-member bots a simple-mode household does not have.
	if !wizardSteps[idx].applies(sess.wizard) {
		http.Redirect(w, r, "/setup/"+nextStep(idx, sess.wizard), http.StatusSeeOther)
		return
	}

	st := sess.wizard
	switch slug {
	case "install":
		st.Container = r.PostFormValue("install") == "container"
		st.DataDir = strings.TrimSpace(r.PostFormValue("data_dir"))
		st.HouseholdName = strings.TrimSpace(r.PostFormValue("household_name"))
		if st.HouseholdName == "" {
			st.HouseholdName = setup.DefaultHouseholdName
		}
		st.MemberNames = splitLines(r.PostFormValue("members"))
		if len(st.MemberNames) == 0 {
			s.wizardError(w, sess, idx, "At least one person, or there is nobody for kenward to talk to.")
			return
		}

	case "admin":
		s.handleAdminStep(w, r, sess)
		return

	case "telegram":
		token := strings.TrimSpace(r.PostFormValue("bot_token"))
		// Assigned before anything is judged, so a refused token is still in the box
		// when the page comes back.
		st.BotToken = token
		st.BotTokenEnv = strings.TrimSpace(r.PostFormValue("bot_token_env"))
		if st.BotTokenEnv == "" {
			st.BotTokenEnv = setup.DefaultBotTokenEnv
		}
		st.WriteEnvile = r.PostFormValue("write_env_file") != ""
		if token != "" && !setup.LooksLikeBotToken(token) && r.PostFormValue("anyway") == "" {
			s.wizardError(w, sess, idx,
				"That does not look like a bot token: BotFather hands out a number, a colon, "+
					"then a long run of letters and digits. Check it, or tick the box below to use it anyway.")
			return
		}
		// Bot privacy mode, asked of Telegram at the one moment it can be fixed
		// cheaply: the token is in hand and the bot is not in any group yet. With
		// privacy mode on — which is Telegram's default for every new bot — the bot
		// receives nothing at all in a group chat, and there is no symptom: nothing
		// arrives, so nothing is logged and the household is simply ignored.
		//
		// A probe that cannot reach Telegram does not stop anybody: the token may be
		// perfectly good and the connection down, and `kenward doctor` asks again.
		if token != "" && r.PostFormValue("no_group") == "" {
			if info, err := s.deps.telegram()(r.Context(), token); err == nil && !info.ReadsGroupMessages {
				s.wizardError(w, sess, idx, botPrivacyRefusal(info.Username))
				return
			}
		}

	case "endpoints":
		st.Endpoints = parseEndpointRows(r)
		switch r.PostFormValue("action") {
		case "probe":
			st.Endpoints = s.probeEndpoints(r.Context(), st.Endpoints)
			s.renderWizard(w, http.StatusOK, sess, idx, page{Flash: "Asked each machine what it is."})
			return
		case "add":
			st.Endpoints = append(st.Endpoints, wizardEndpoint{})
			s.renderWizard(w, http.StatusOK, sess, idx, page{})
			return
		}
		if len(st.Endpoints) == 0 {
			s.wizardError(w, sess, idx, "At least one endpoint, or nothing can answer.")
			return
		}
		for _, e := range st.Endpoints {
			if e.Name == "" || e.BaseURL == "" || e.Model == "" {
				s.wizardError(w, sess, idx, "Every endpoint needs a name, an address and a model.")
				return
			}
		}

	case "trust":
		mode := config.Mode(r.PostFormValue("mode"))
		if mode != config.ModeSimple && mode != config.ModeIsolated {
			s.wizardError(w, sess, idx, "Choose one of the two.")
			return
		}
		st.Mode = mode

	case "bots":
		st.readMemberSecrets(r)
		if err := checkMemberSecrets(st); err != nil {
			s.wizardError(w, sess, idx, err.Error())
			return
		}

	case "advanced":
		// Everything the operator typed is recorded before anything is judged, so a
		// refusal re-renders the form they submitted rather than the form they
		// started from. The telegram step does the same with a rejected token, and
		// for the same reason: an answer thrown away by the page that refused it is
		// an answer given twice, or given up on.
		st.Agents = config.Agents(strings.TrimSpace(r.PostFormValue("agents")))
		st.GroupChatID = strings.TrimSpace(r.PostFormValue("group_chat_id"))
		st.Persona = config.PersonaConfig{
			Language:  strings.TrimSpace(r.PostFormValue("persona_language")),
			Tone:      strings.TrimSpace(r.PostFormValue("persona_tone")),
			Character: strings.TrimSpace(r.PostFormValue("persona_character")),
		}
		st.SearchLimit = atoi(r.PostFormValue("search_limit"), config.DefaultSearchLimit)
		st.MaxProposals = atoi(r.PostFormValue("max_proposals"), config.DefaultMaxProposalsPerTurn)
		st.HistoryReset = strings.TrimSpace(r.PostFormValue("history_reset"))
		st.IdleTimeout = strings.TrimSpace(r.PostFormValue("idle_timeout"))
		st.UpdateChannel = strings.TrimSpace(r.PostFormValue("update_channel"))
		st.CloudEveryone = r.PostFormValue("cloud_everyone") != ""

		agents, err := checkAgents(string(st.Agents), st.Mode, wizardModeRemedy)
		if err != nil {
			s.wizardError(w, sess, idx, err.Error())
			return
		}
		st.Agents = agents
		// Said here rather than left to config.Validate, which would answer somebody
		// looking at a text box with a field path. The limit is real: persona text
		// rides in every prompt and is the one part of it the context budget never
		// trims, so a long one is paid for out of retrieved memory.
		if err := checkPersonaLengths(st.Persona); err != nil {
			s.wizardError(w, sess, idx, err.Error())
			return
		}
		if _, err := checkGroupChat(st.Agents, st.GroupChatID); err != nil {
			s.wizardError(w, sess, idx, err.Error())
			return
		}

	case "review":
		s.finishWizard(w, r, sess)
		return
	}

	http.Redirect(w, r, "/setup/"+nextStep(idx, st), http.StatusSeeOther)
}

func (s *Server) wizardError(w http.ResponseWriter, sess *session, idx int, msg string) {
	s.renderWizard(w, http.StatusBadRequest, sess, idx, page{Error: msg})
}

// handleAdminStep creates the account and upgrades the session in one action.
//
// Everything that has to happen at this moment happens here, in this order, and the
// order is the security property: the account is created, the setup token is discarded,
// every setup session is destroyed, and a brand new admin session is issued. Nothing is
// promoted in place — the id the browser had before this request is not the id it has
// after it, which is what makes session fixation not a thing that can be attempted.
func (s *Server) handleAdminStep(w http.ResponseWriter, r *http.Request, sess *session) {
	idx := stepIndex("admin")
	if s.admin.Exists() {
		// Somebody replayed the form. There is no reset flow and this is not one.
		http.Error(w, "an admin account already exists", http.StatusConflict)
		return
	}
	pw := r.PostFormValue("password")
	if pw != r.PostFormValue("password2") {
		s.wizardError(w, sess, idx, "The two passwords are not the same.")
		return
	}
	if err := s.admin.Create(r.Context(), pw); err != nil {
		s.wizardError(w, sess, idx, err.Error())
		return
	}

	// The token is already gone — Redeem removed it — and this is the belt to that
	// pair of braces. From here the setup routes do not exist, so a token that
	// survived would be a live credential for a door that has been bricked up.
	if err := s.tokens.Discard(); err != nil {
		s.deps.logger().Error("dashboard", "event", "setup_token_not_discarded", "err", err.Error())
	}

	state := sess.wizard
	s.sess.destroyKind(kindSetup)
	clearCookie(w, setupCookieName, s.secure())

	admin, err := s.sess.create(kindAdmin)
	if err != nil {
		http.Error(w, "the account was created but a session could not be started; sign in", http.StatusInternalServerError)
		return
	}
	admin.wizard = state
	setCookie(w, adminCookieName, admin.id, s.secure())
	s.guard.succeed()
	s.deps.logger().Info("dashboard", "event", "admin_created")

	// A household that already had a kenward.yaml wanted an account, not a new
	// configuration. Carrying on through the wizard would end in a refusal to
	// overwrite their file, four screens later.
	if _, err := os.Stat(s.deps.ConfigPath); err == nil {
		redirectWith(w, r, "/overview", "Admin account created. Your existing configuration was left alone.")
		return
	}
	http.Redirect(w, r, "/setup/telegram", http.StatusSeeOther)
}

// finishWizard creates the lore spaces, writes the configuration, and turns the
// dashboard on.
//
// The spaces are created here rather than earlier because this is the first irreversible
// step, and lore has no delete for a space. Everything before this can be gone back on
// by closing the tab.
func (s *Server) finishWizard(w http.ResponseWriter, r *http.Request, sess *session) {
	idx := stepIndex("review")
	st := sess.wizard

	if _, err := os.Stat(s.deps.ConfigPath); err == nil {
		s.wizardError(w, sess, idx, fmt.Sprintf(
			"There is already a configuration at %s. This wizard writes a new one and will not "+
				"replace it; edit the existing one under Settings, or move it aside and start again.",
			s.deps.ConfigPath))
		return
	}
	// Belt to the bots step's braces, and the last place it can be caught: the file
	// about to be written names two variables per member, and writing it without their
	// values produces a household that cannot start. The step refuses a blank; this
	// refuses a flow that reached here without passing through it — a step is a screen,
	// and a screen is not a guarantee that a POST went through it. Synced first, so a
	// household that never rendered the step is judged as the four blanks it is rather
	// than as an empty list with nothing to complain about.
	if st.Mode == config.ModeIsolated {
		st.syncMemberSecrets()
	}
	if err := checkMemberSecrets(st); err != nil {
		s.wizardError(w, sess, idx, err.Error())
		return
	}

	lore, err := s.openLore(r)
	if err != nil {
		s.wizardError(w, sess, idx, "lore could not be reached, and every space kenward uses has to be made in it: "+err.Error())
		return
	}
	defer lore.Close()

	spaces := map[string]string{}
	shared, err := lore.CreateSpace(r.Context(), st.HouseholdName+" — household")
	if err != nil {
		s.wizardError(w, sess, idx, "creating the household's shared memory: "+spaceExistsHint(err))
		return
	}
	spaces[householdSpaceKey] = shared.ID
	for _, name := range st.MemberNames {
		sp, err := lore.CreateSpace(r.Context(), st.HouseholdName+" — "+name)
		if err != nil {
			s.wizardError(w, sess, idx, fmt.Sprintf("creating %s's private memory: %s", name, spaceExistsHint(err)))
			return
		}
		spaces[setup.Slugify(name)] = sp.ID
	}

	dataDir := st.DataDir
	if st.Container && dataDir == "" {
		dataDir = "/var/lib/kenward"
	}
	wiz := setup.New(setup.NewScriptIO(), setup.Options{
		ConfigPath: s.deps.ConfigPath,
		DataDir:    dataDir,
		GOOS:       s.deps.goos(),
		Probe:      s.deps.probe(),
		Spaces:     lore.Spaces,
		Answers:    st.answers(spaces),
	})
	cfg, err := wiz.Run(r.Context())
	if err != nil {
		s.wizardError(w, sess, idx, err.Error())
		return
	}
	if err := st.applyAdvanced(cfg); err != nil {
		s.wizardError(w, sess, idx, err.Error())
		return
	}

	// The dashboard turns itself on, on loopback. It is the only setting this wizard
	// decides on the operator's behalf, and it is the safe one: the household has
	// just used the dashboard to set itself up, so a configuration that then refused
	// to serve it would be a trick. Anything wider than loopback is chosen under
	// Access, afterwards, with the account already in place.
	cfg.Dashboard = config.DashboardConfig{
		Enabled:  true,
		Bind:     s.dash.BindAddr(),
		Exposure: config.ExposureLoopback,
	}
	if !cfg.Dashboard.Loopback() {
		// The process was started bound somewhere else. Write what is true.
		cfg.Dashboard.Bind = config.DefaultDashboardBind
	}
	if err := setup.WriteConfig(s.deps.ConfigPath, cfg, true); err != nil {
		s.wizardError(w, sess, idx, err.Error())
		return
	}

	sess.wizard = nil
	// What was written, and the one thing between it and a household that starts. The
	// process serving this page was started before the .env existed and cannot see it,
	// so Overview is about to list every secret in it as unreadable — which is true of
	// this process and not of the node the operator is about to start. Saying so here
	// is cheaper than letting them find a wall of red under a line that said "written".
	done := "Written " + s.deps.ConfigPath + "."
	if p := wiz.EnvFilePath(); p != "" {
		done += " The secrets you gave are in " + p + " — load it into kenward's environment" +
			" before restarting (env_file: in compose, EnvironmentFile= in a unit, or" +
			" `set -a; . ./.env; set +a` in a shell). This process cannot see it, so they are" +
			" listed below as unreadable until kenward is restarted with it."
	}
	done += " Restart kenward for it to take effect, then invite the household."
	redirectWith(w, r, "/overview", done)
}

// ------------------------------------------------------------------- login

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.admin.Exists() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, ok := s.sessionFrom(r, adminCookieName, kindAdmin); ok {
		http.Redirect(w, r, "/overview", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", page{Title: "Sign in", Flash: flashOf(r)})
}

// handleLoginSubmit authenticates.
//
// The lockout is checked first, before the password is looked at, so a locked account
// costs an attacker an Argon2id derivation of nothing. Every rejection says the same
// thing — there is one account, so there is no username to enumerate and nothing a
// distinct message could helpfully tell anyone.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if locked, until := s.guard.locked(); locked {
		s.deps.logger().Warn("dashboard", "event", "login_locked_out")
		s.render(w, http.StatusTooManyRequests, "login", page{
			Title: "Sign in",
			Error: fmt.Sprintf("Too many failed attempts. Try again in %s.", untilText(until, s.deps.now())),
		})
		return
	}

	if err := s.admin.Verify(r.Context(), r.PostFormValue("password")); err != nil {
		s.guard.fail()
		s.deps.logger().Warn("dashboard", "event", "login_failed")
		s.render(w, http.StatusUnauthorized, "login", page{
			Title: "Sign in",
			Error: "That is not the admin password.",
		})
		return
	}

	// A fresh id, always. Nothing is promoted, and any session the browser arrived
	// holding is not the one it leaves with.
	if c, err := r.Cookie(adminCookieName); err == nil {
		s.sess.destroy(c.Value)
	}
	sess, err := s.sess.create(kindAdmin)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.guard.succeed()
	setCookie(w, adminCookieName, sess.id, s.secure())
	s.deps.logger().Info("dashboard", "event", "login_ok")
	http.Redirect(w, r, "/overview", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, sess *session) {
	s.sess.destroy(sess.id)
	clearCookie(w, adminCookieName, s.secure())
	redirectWith(w, r, "/login", "Signed out.")
}

func untilText(until, now time.Time) string {
	d := until.Sub(now).Round(time.Minute)
	if d < time.Minute {
		return "a minute"
	}
	return d.String()
}

// ---------------------------------------------------------------- overview

type overviewData struct {
	ConfigPath string
	Configured bool
	Problems   []string
	Missing    []string
	Mode       string
	Household  string
	Members    string
	Enrolled   string
	Endpoints  string
	Bind       string
	Statement  string
	Exposure   string
	TierNotes  []string
	SetupToken bool
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, sess *session) {
	d := overviewData{ConfigPath: s.deps.ConfigPath}
	cfg, err := s.loadConfig()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.render(w, http.StatusOK, "overview", page{
			Title: "Overview", Nav: "overview", CSRF: sess.csrf, SignedIn: true,
			Flash: flashOf(r), Data: d,
		})
		return
	case err != nil:
		d.Problems = []string{err.Error()}
		s.render(w, http.StatusOK, "overview", page{
			Title: "Overview", Nav: "overview", CSRF: sess.csrf, SignedIn: true, Data: d,
		})
		return
	}

	d.Configured = true
	d.Mode = string(cfg.Mode)
	d.Household = cfg.Household.Name
	enrolled := 0
	for _, m := range cfg.DomainMembers() {
		if m.Enrolled() {
			enrolled++
		}
	}
	d.Members = humanCount(len(cfg.Members), "member", "members")
	d.Enrolled = humanCount(enrolled, "has claimed an invite", "have claimed an invite")
	d.Endpoints = humanCount(len(cfg.Endpoints), "endpoint", "endpoints")
	d.Bind = cfg.Dashboard.DashboardSummary()
	d.Statement = privacy.Statement(privacyModeFor(cfg.Mode))
	d.Exposure = privacy.DashboardNote(ReachFor(cfg.Dashboard), s.URL(), cfg.Dashboard.TLS())

	local := setup.LocalTiers(cfg.Endpoints)
	for _, m := range cfg.DomainMembers() {
		if m.SharedOnly {
			// The statement above promises a private memory, and this member has
			// none. Appended once, on the first one found, for the same reason
			// doctor appends it: the claim and the exception belong in one block
			// or a reader can finish the claim without meeting the exception.
			d.Statement += "\n\n" + privacy.SharedOnlyNote()
			break
		}
	}
	for _, m := range cfg.DomainMembers() {
		if m.SharedOnly {
			// privacy.MemberNote is about a member's own tier chain, and this
			// member has none. The household's own line, below, is theirs.
			continue
		}
		d.TierNotes = append(d.TierNotes, privacy.MemberNote(m, setup.StaysHome(local, m.Tiers)))
	}
	if cfg.Household.Name != "" {
		d.TierNotes = append(d.TierNotes,
			privacy.TierNote(cfg.Household.Name, cfg.Household.Tiers, setup.StaysHome(local, cfg.Household.Tiers)))
	}

	if err := cfg.ValidateForUnit(nil, config.UnitScope{NoSecrets: true}); err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			d.Problems = ve.Problems
		} else {
			d.Problems = []string{err.Error()}
		}
	}
	d.Missing = cfg.MissingSecretNamesForUnit(config.NewSecrets(config.SecretOptions{}), config.UnitScope{})

	s.render(w, http.StatusOK, "overview", page{
		Title: "Overview", Nav: "overview", CSRF: sess.csrf, SignedIn: true,
		Flash: flashOf(r), Data: d,
	})
}

// ---------------------------------------------------------------- members

type memberRow struct {
	ID       string
	Name     string
	Space    string
	Tiers    string
	Enrolled bool
	Note     string
}

type membersData struct {
	Configured bool
	Members    []memberRow
	Tiers      []string
	Mode       string
	// Code is a claim code that was just minted, shown once. It arrives through the
	// session rather than through the URL: a code in a query string is a code in the
	// browser's history and in any log the operator's own machine keeps.
	Code     string
	CodeFor  string
	CodeNote string
}

func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, err := s.loadConfig()
	if err != nil {
		s.render(w, http.StatusOK, "members", page{
			Title: "Members", Nav: "members", CSRF: sess.csrf, SignedIn: true,
			Error: "There is no configuration to add members to yet: " + err.Error(),
			Data:  membersData{},
		})
		return
	}
	s.render(w, http.StatusOK, "members", page{
		Title: "Members", Nav: "members", CSRF: sess.csrf, SignedIn: true,
		Flash: flashOf(r),
		Data:  s.membersData(cfg, sess),
	})
}

func (s *Server) membersData(cfg *config.Config, sess *session) membersData {
	local := setup.LocalTiers(cfg.Endpoints)
	d := membersData{Configured: true, Mode: string(cfg.Mode)}
	for tier := range local {
		d.Tiers = append(d.Tiers, tier)
	}
	sortStrings(d.Tiers)
	for _, m := range cfg.Members {
		row := memberRow{
			ID: m.ID, Name: m.Name, Space: m.PrivateSpace,
			Tiers:    strings.Join(m.Tiers, ", "),
			Enrolled: m.TelegramID != 0,
		}
		switch {
		case m.SharedOnly:
			// Neither of the two notes below is about them: both describe where a
			// member's own conversations may travel, and every conversation this
			// member has is the household's and travels on the household's chain.
			row.Space = "none — the household's shared memory"
			row.Tiers = strings.Join(cfg.Household.Tiers, ", ")
			row.Note = "no memory of their own; everything they say that is remembered is the household's"
		case setup.StaysHome(local, m.Tiers):
			row.Note = "will refuse rather than use a provider"
		default:
			row.Note = "may use a provider"
		}
		d.Members = append(d.Members, row)
	}
	return d
}

// handleMemberAdd is the whole of adding somebody: their lore space is created, they are
// written into kenward.yaml, and their claim code is minted and shown once.
//
// Doing it in one action is the point of the feature. Every one of these three steps
// used to be a separate command with a separate way of going wrong, and the commonest
// outcome was a member declared in a file with a space that did not exist, discovered
// weeks later when their first retrieval came back empty.
func (s *Server) handleMemberAdd(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, err := s.loadConfig()
	if err != nil {
		http.Error(w, "there is no configuration to add a member to", http.StatusConflict)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.membersError(w, sess, cfg, "A name, as they are known in the household.")
		return
	}
	// A member with no memory of their own: no private space, no tier chain of their
	// own, no bot and no pod. Read before the tier check, because the tier check does
	// not apply to them — they have no private conversations to have a policy about,
	// and their conversations run on the household's chain.
	sharedOnly := r.PostFormValue("shared_only") != ""
	tiers := r.PostForm["tiers"]
	if sharedOnly {
		tiers = nil
	}
	if len(tiers) == 0 && !sharedOnly {
		// No default, ever. A tier chain is that member's privacy policy and
		// nothing here chooses one on their behalf — the same rule the terminal
		// wizard and `kenward invite`'s help text both hold to.
		s.membersError(w, sess, cfg, "Choose where this person's private conversations may go. There is no default: the chain is their privacy policy.")
		return
	}

	id := setup.Slugify(name)
	if id == "" {
		s.membersError(w, sess, cfg, fmt.Sprintf("%q does not reduce to an id kenward can use; give a name with some letters or digits in it.", name))
		return
	}
	for _, m := range cfg.Members {
		if m.ID == id {
			s.membersError(w, sess, cfg, fmt.Sprintf("There is already a member with the id %q.", id))
			return
		}
	}

	member := config.MemberConfig{ID: id, Name: name, Tiers: tiers}
	if sharedOnly {
		// lore is not opened at all. There is no space to make, and opening the
		// store to make nothing would fail this action on a lore outage for a
		// member who does not need lore to exist.
		member.SharedOnly = true
	} else {
		lore, err := s.openLore(r)
		if err != nil {
			s.membersError(w, sess, cfg, "lore could not be reached, and a private space has to be made in it: "+err.Error())
			return
		}
		defer lore.Close()

		label := name
		if cfg.Household.Name != "" {
			label = cfg.Household.Name + " — " + name
		}
		space, err := lore.CreateSpace(r.Context(), label)
		if err != nil {
			s.membersError(w, sess, cfg, "creating their private memory: "+spaceExistsHint(err))
			return
		}
		member.PrivateSpace = space.ID
		if cfg.Mode == config.ModeIsolated {
			member.BotTokenEnv = envVarFor(setup.MemberBotTokenPrefix, id)
			member.PassphraseEnv = envVarFor(setup.MemberPassphrasePrefix, id)
		}
	}
	cfg.Members = append(cfg.Members, member)

	if err := cfg.ValidateForUnit(nil, config.UnitScope{NoSecrets: true}); err != nil {
		s.membersError(w, sess, cfg, "adding them would produce a configuration kenward refuses: "+err.Error())
		return
	}
	if err := setup.WriteConfig(s.deps.ConfigPath, cfg, true); err != nil {
		s.membersError(w, sess, cfg, err.Error())
		return
	}

	code, err := s.mint(r, cfg, domain.MemberID(id), name)
	if err != nil {
		redirectWith(w, r, "/members", fmt.Sprintf(
			"%s was added and their space created, but the claim code could not be minted (%v). Use Invite on their row.", name, err))
		return
	}
	s.showCode(w, r, sess, cfg, name, code)
}

// handleMemberInvite mints a code for somebody already declared.
func (s *Server) handleMemberInvite(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, err := s.loadConfig()
	if err != nil {
		http.Error(w, "there is no configuration", http.StatusConflict)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	m, ok := cfg.MemberByID(domain.MemberID(id))
	if !ok {
		s.membersError(w, sess, cfg, "No member with that id.")
		return
	}
	if m.Enrolled() {
		s.membersError(w, sess, cfg, m.Name+" has already claimed an invite. Revoke first if they need to bind a different Telegram account.")
		return
	}
	code, err := s.mint(r, cfg, m.ID, m.Name)
	if err != nil {
		s.membersError(w, sess, cfg, err.Error())
		return
	}
	s.showCode(w, r, sess, cfg, m.Name, code)
}

func (s *Server) mint(r *http.Request, cfg *config.Config, id domain.MemberID, name string) (string, error) {
	if s.deps.MintInvite == nil {
		return "", errors.New("this build has no way to mint a claim code")
	}
	return s.deps.MintInvite(r.Context(), cfg, id, name, enrol.DefaultTTL)
}

// showCode renders the claim code once, on the page that minted it.
//
// It is not redirected to and it is not in a URL. This is the only moment the code
// exists in the clear — the store holds a digest — so putting it in a query string would
// leave the one recoverable copy in the browser's history.
func (s *Server) showCode(w http.ResponseWriter, r *http.Request, sess *session, cfg *config.Config, name, code string) {
	d := s.membersData(cfg, sess)
	d.Code = code
	d.CodeFor = name
	d.CodeNote = fmt.Sprintf("It works once and expires in %s. Until they use it, the bot will not reply to them at all.",
		enrol.DefaultTTL)
	if cfg.Mode == config.ModeIsolated {
		d.CodeNote += fmt.Sprintf(" This household is isolated, so the code reaches %s's pod when that pod is next created; restart kenward before handing it over.", name)
	}
	s.render(w, http.StatusOK, "members", page{
		Title: "Members", Nav: "members", CSRF: sess.csrf, SignedIn: true,
		Flash: name + " is set up. Hand them this code in person, not in a chat.",
		Data:  d,
	})
}

func (s *Server) handleMemberRevoke(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, err := s.loadConfig()
	if err != nil {
		http.Error(w, "there is no configuration", http.StatusConflict)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	m, ok := cfg.MemberByID(domain.MemberID(id))
	if !ok {
		s.membersError(w, sess, cfg, "No member with that id.")
		return
	}
	if s.deps.Revoke == nil {
		s.membersError(w, sess, cfg, "this build has no way to revoke")
		return
	}
	if err := s.deps.Revoke(r.Context(), cfg, m.ID); err != nil {
		s.membersError(w, sess, cfg, err.Error())
		return
	}
	redirectWith(w, r, "/members", m.Name+" is unbound. Their memory is untouched; mint a new code when they need to bind again.")
}

// spaceExistsHint renders a space-creation failure, and turns the one that is a
// person's mistake rather than a fault into an instruction.
//
// lore refuses a name another space already holds and creates nothing. It is
// deliberate that kenward does not then reuse the existing space: this id becomes a
// member's private memory, and quietly attaching a new member to a space that was
// already there is how one person's memory becomes another's. So the only sound
// answer is a different name, and this is where a person is told that.
func spaceExistsHint(err error) string {
	if errors.Is(err, memory.ErrSpaceExists) {
		return err.Error() + ". Spaces are never reused for this — that would attach them to " +
			"memory somebody else may already be using — so give them a name no space in this " +
			"lore has yet, or configure the existing space's id by hand."
	}
	return err.Error()
}

func (s *Server) membersError(w http.ResponseWriter, sess *session, cfg *config.Config, msg string) {
	s.render(w, http.StatusBadRequest, "members", page{
		Title: "Members", Nav: "members", CSRF: sess.csrf, SignedIn: true,
		Error: msg, Data: s.membersData(cfg, sess),
	})
}

// envVarFor builds a per-member environment variable name. It mirrors internal/setup's
// own rule, which is unexported there; the two must agree, and a member added here whose
// variable is spelled differently is a pod with no token.
func envVarFor(prefix, id string) string {
	return prefix + "_" + strings.ToUpper(strings.ReplaceAll(setup.Slugify(id), "-", "_"))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}
