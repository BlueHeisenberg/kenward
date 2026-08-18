package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/privacy"
	"github.com/BlueHeisenberg/kenward/internal/setup"
)

// ---------------------------------------------------------------- settings

type settingsData struct {
	Cfg        *config.Config
	ConfigPath string
	Problems   []string
	// Spaces is what lore holds, so a space can be chosen from a list rather than
	// pasted as a UUID. Empty when lore could not be reached, which is not an error
	// here: the ids already in the file still work.
	Spaces   []memory.Space
	LoreErr  string
	Channels []config.UpdateChannel
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, err := s.loadConfig()
	if err != nil {
		s.render(w, http.StatusOK, "settings", page{
			Title: "Settings", Nav: "settings", CSRF: sess.csrf, SignedIn: true,
			Error: "There is nothing to edit yet: " + err.Error(),
			Data:  settingsData{ConfigPath: s.deps.ConfigPath},
		})
		return
	}
	s.render(w, http.StatusOK, "settings", page{
		Title: "Settings", Nav: "settings", CSRF: sess.csrf, SignedIn: true,
		Flash: flashOf(r),
		Data:  s.settingsData(r.Context(), cfg),
	})
}

func (s *Server) settingsData(ctx context.Context, cfg *config.Config) settingsData {
	d := settingsData{
		Cfg:        cfg,
		ConfigPath: s.deps.ConfigPath,
		Channels:   []config.UpdateChannel{config.UpdateStable, config.UpdateEdge, config.UpdateOff},
	}
	if s.deps.Lore != nil {
		if lore, err := s.deps.Lore(ctx); err == nil {
			defer lore.Close()
			if spaces, err := lore.Spaces(ctx); err == nil {
				d.Spaces = spaces
			} else {
				d.LoreErr = err.Error()
			}
		} else {
			d.LoreErr = err.Error()
		}
	}
	return d
}

// handleSettingsSubmit rewrites kenward.yaml from the form.
//
// It edits the configuration that was on disk rather than building one from the form
// alone, so a key this page does not show — a secret's file form, a member's telegram id
// — survives the edit rather than being silently dropped by a form that never asked
// about it.
func (s *Server) handleSettingsSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, err := s.loadConfig()
	if err != nil {
		http.Error(w, "there is no configuration to edit", http.StatusConflict)
		return
	}

	if err := applySettingsForm(cfg, r); err != nil {
		s.settingsError(w, r, sess, cfg, err.Error())
		return
	}
	cfg.ApplyDefaults()

	if err := cfg.ValidateForUnit(nil, config.UnitScope{NoSecrets: true}); err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			d := s.settingsData(r.Context(), cfg)
			d.Problems = ve.Problems
			s.render(w, http.StatusBadRequest, "settings", page{
				Title: "Settings", Nav: "settings", CSRF: sess.csrf, SignedIn: true,
				Error: "Nothing was written. kenward would refuse this configuration:",
				Data:  d,
			})
			return
		}
		s.settingsError(w, r, sess, cfg, err.Error())
		return
	}

	if err := setup.WriteConfig(s.deps.ConfigPath, cfg, true); err != nil {
		s.settingsError(w, r, sess, cfg, err.Error())
		return
	}
	redirectWith(w, r, "/settings", "Written "+s.deps.ConfigPath+". Restart kenward for it to take effect.")
}

func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, sess *session, cfg *config.Config, msg string) {
	s.render(w, http.StatusBadRequest, "settings", page{
		Title: "Settings", Nav: "settings", CSRF: sess.csrf, SignedIn: true,
		Error: msg, Data: s.settingsData(r.Context(), cfg),
	})
}

// applySettingsForm folds the form into a configuration.
//
// Members and endpoints are addressed by index, in the order the page rendered them,
// because that is the order the file holds them in and the page is a view of the file.
//
// Every field the first-run wizard can set appears here, and so do the two the
// memory-policy work added and the wizard does not ask about — memory.announce_reads and
// capture.private_writes. They are both things a household changes its mind about after
// living with the assistant for a week, which is exactly what this page is for, and
// D-039's parity rule cuts the other way too: a setting a headless operator can edit in
// kenward.yaml and an admin cannot see is a setting that only half exists.
func applySettingsForm(cfg *config.Config, r *http.Request) error {
	cfg.Household.Name = strings.TrimSpace(r.PostFormValue("household_name"))
	cfg.Household.SharedSpace = strings.TrimSpace(r.PostFormValue("shared_space"))
	cfg.Household.Tiers = splitList(r.PostFormValue("household_tiers"))
	cfg.Telegram.BotTokenEnv = strings.TrimSpace(r.PostFormValue("bot_token_env"))

	// The identity question and kenward's persona, editable here for the whole life
	// of the household: an answer available only in the first sixty seconds of setup
	// is an answer people get wrong. A member's own persona is not on this page and
	// must not be — it is theirs, written in Telegram, and an admin form that could
	// overwrite it would make "your assistant is yours" false.
	agents, err := checkAgents(r.PostFormValue("agents"), cfg.Mode, settingsModeRemedy)
	if err != nil {
		return err
	}
	cfg.Household.Agents = agents
	// Read after the identity answer, because whether a blank one is allowed depends on
	// it. The same refusal both wizards make, from the same rule, on the one page where
	// the remedy is a field the reader is already looking at: under one assistant each
	// both of kenward's own conversations — the group, and each member's private chat
	// with kenward — are built off this id, so a household without one has no kenward in
	// it. Blank is still how a group is unmapped under one shared assistant, which
	// answers every private chat either way.
	groupChat, err := checkGroupChat(cfg.Household.Agents, r.PostFormValue("group_chat_id"))
	if err != nil {
		return err
	}
	cfg.Household.GroupChatID = groupChat
	cfg.Household.Persona = config.PersonaConfig{
		Language:  strings.TrimSpace(r.PostFormValue("persona_language")),
		Tone:      strings.TrimSpace(r.PostFormValue("persona_tone")),
		Character: strings.TrimSpace(r.PostFormValue("persona_character")),
	}
	if err := checkPersonaLengths(cfg.Household.Persona); err != nil {
		return err
	}

	for i := range cfg.Members {
		p := fmt.Sprintf("member.%d.", i)
		if v := strings.TrimSpace(r.PostFormValue(p + "name")); v != "" {
			cfg.Members[i].Name = v
		}
		if v := strings.TrimSpace(r.PostFormValue(p + "private_space")); v != "" {
			cfg.Members[i].PrivateSpace = v
		}
		// A tier chain may not be blanked here. Validation refuses an empty chain
		// anyway, and refusing at the form says why in the operator's own terms
		// rather than as a field path.
		if tiers := splitList(r.PostFormValue(p + "tiers")); len(tiers) > 0 {
			cfg.Members[i].Tiers = tiers
		} else {
			return fmt.Errorf("%s has no tier chain, and there is no default: the chain is that member's privacy policy", cfg.Members[i].Name)
		}
	}

	for i := range cfg.Endpoints {
		p := fmt.Sprintf("endpoint.%d.", i)
		if v := strings.TrimSpace(r.PostFormValue(p + "name")); v != "" {
			cfg.Endpoints[i].Name = v
		}
		if v := strings.TrimSpace(r.PostFormValue(p + "base_url")); v != "" {
			cfg.Endpoints[i].BaseURL = v
		}
		if v := strings.TrimSpace(r.PostFormValue(p + "model")); v != "" {
			cfg.Endpoints[i].Model = v
		}
		cfg.Endpoints[i].APIKeyEnv = strings.TrimSpace(r.PostFormValue(p + "api_key_env"))
		if tags := splitList(r.PostFormValue(p + "tags")); len(tags) > 0 {
			cfg.Endpoints[i].Tags = tags
		}
		if d, err := parseDuration(r.PostFormValue(p + "timeout")); err != nil {
			return fmt.Errorf("%s: timeout: %w", cfg.Endpoints[i].Name, err)
		} else if d > 0 {
			cfg.Endpoints[i].Timeout = config.Duration(d)
		}
		cfg.Endpoints[i].ContextWindow = atoi(r.PostFormValue(p+"context_window"), cfg.Endpoints[i].ContextWindow)
		cfg.Endpoints[i].MaxCompletionTokens = atoi(r.PostFormValue(p+"max_completion_tokens"), cfg.Endpoints[i].MaxCompletionTokens)
	}

	cfg.Memory.SearchLimit = atoi(r.PostFormValue("search_limit"), cfg.Memory.SearchLimit)
	// Assigned unconditionally, exactly like idle_timeout below and for the same
	// reason: zero is a meaning here, so an empty box has to be able to turn the
	// schedule off.
	if d, err := parseDuration(r.PostFormValue("history_reset")); err != nil {
		return fmt.Errorf("conversation reset: %w", err)
	} else if d > config.MaxHistoryReset {
		return fmt.Errorf("conversation reset: %s is longer than %s; resets are counted from midnight, so a longer gap cannot be kept — use 0s for off", d, config.MaxHistoryReset)
	} else {
		cfg.History.ResetEvery = config.Duration(d)
	}
	if d, err := parseDuration(r.PostFormValue("idle_timeout")); err != nil {
		return fmt.Errorf("session idle timeout: %w", err)
	} else {
		cfg.Session.IdleTimeout = config.Duration(d)
	}
	cfg.Capture.MaxProposalsPerTurn = atoi(r.PostFormValue("max_proposals"), cfg.Capture.MaxProposalsPerTurn)
	// Written explicitly rather than left unset, because a checkbox cannot express
	// "not stated": an unticked box posts nothing, which is indistinguishable from a
	// field this form never had. The pointer is what tells those apart in the file,
	// and once an admin has looked at this control the household has an opinion.
	announceReads := r.PostFormValue("announce_reads") != ""
	cfg.Memory.AnnounceReads = &announceReads
	// The only capture policy a household may set. There is deliberately no control
	// for the shared space's confirmation or for the write announcement, because
	// neither has a configuration key to bind one to — see config.CaptureConfig. An
	// unrecognised value cannot arrive from the select and is refused by validation
	// anyway, which is where a hand-edited POST meets the same rule the file does.
	if v := strings.TrimSpace(r.PostFormValue("private_writes")); v != "" {
		cfg.Capture.PrivateWrites = config.PrivateWrites(v)
	}

	if v := strings.TrimSpace(r.PostFormValue("update_channel")); v != "" {
		cfg.Update.Channel = config.UpdateChannel(v)
	}
	if d, err := parseDuration(r.PostFormValue("check_interval")); err != nil {
		return fmt.Errorf("update check interval: %w", err)
	} else if d > 0 {
		cfg.Update.CheckInterval = config.Duration(d)
	}
	return nil
}

// parseDuration reads a duration field. Empty is zero, which is a meaningful value for
// session.idle_timeout and means "take the default" for everything else.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a length of time; write it as 30s, 5m or 6h", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return d, nil
}

// ---------------------------------------------------------------- exposure

type exposureData struct {
	Cfg config.DashboardConfig
	// Current is the address this process actually bound, which may differ from the
	// file if the file has been edited since it started.
	Current string
	// Interfaces are the addresses this machine has, so a tailnet address can be
	// chosen rather than typed.
	Interfaces []ifaceOption
	Note       string
	// Fingerprint is shown once, right after a certificate is generated.
	Fingerprint string
	Restart     bool
}

type ifaceOption struct {
	Name    string
	Addr    string
	Kind    string
	Tailnet bool
}

func (s *Server) handleExposure(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, err := s.loadConfig()
	if err != nil {
		s.render(w, http.StatusOK, "exposure", page{
			Title: "Access", Nav: "exposure", CSRF: sess.csrf, SignedIn: true,
			Error: "There is no configuration to change yet: " + err.Error(),
			Data:  exposureData{Cfg: s.dash, Current: s.Addr()},
		})
		return
	}
	s.render(w, http.StatusOK, "exposure", page{
		Title: "Access", Nav: "exposure", CSRF: sess.csrf, SignedIn: true,
		Flash: flashOf(r),
		Data: exposureData{
			Cfg:        cfg.Dashboard,
			Current:    s.Addr(),
			Interfaces: interfaceOptions(),
			Note:       privacy.DashboardNote(ReachFor(cfg.Dashboard), s.URL(), cfg.Dashboard.TLS()),
		},
	})
}

// handleExposureSubmit records a new exposure, generating a certificate when the chosen
// one needs one.
//
// It writes the configuration and asks for a restart rather than rebinding underneath
// the request that asked for it. Rebinding live would drop the connection the operator
// is reading the confirmation on, and — worse — would leave a household whose file says
// one thing and whose socket does another until somebody restarted anyway.
//
// ponytail: config write plus a restart, not a live rebind. Rebind in place if changing
// exposure ever stops being a once-a-year action.
func (s *Server) handleExposureSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	cfg, err := s.loadConfig()
	if err != nil {
		http.Error(w, "there is no configuration", http.StatusConflict)
		return
	}

	exposure := config.Exposure(strings.TrimSpace(r.PostFormValue("exposure")))
	next := config.DashboardConfig{Enabled: true, Exposure: exposure}
	fingerprint := ""

	switch exposure {
	case config.ExposureLoopback:
		next.Bind = net.JoinHostPort("127.0.0.1", portOf(cfg.Dashboard.BindAddr()))

	case config.ExposureTailnet, config.ExposureLAN:
		host := strings.TrimSpace(r.PostFormValue("address"))
		if host == "" {
			s.exposureError(w, r, sess, cfg, "Choose which address to listen on.")
			return
		}
		port := strings.TrimSpace(r.PostFormValue("port"))
		if port == "" {
			port = portOf(cfg.Dashboard.BindAddr())
		}
		if _, err := strconv.Atoi(port); err != nil {
			s.exposureError(w, r, sess, cfg, fmt.Sprintf("%q is not a port number.", port))
			return
		}
		next.Bind = net.JoinHostPort(host, port)

		if exposure == config.ExposureLAN {
			// Generated here rather than asked for. LAN exposure requires TLS and
			// a household is not going to have a certificate lying about; the one
			// honest thing to do is make one and show its fingerprint so it can be
			// checked once against the browser's warning.
			if err := s.ensureDir(); err != nil {
				s.exposureError(w, r, sess, cfg, err.Error())
				return
			}
			certFile, keyFile := s.certPaths()
			fp, err := generateSelfSigned(certFile, keyFile, host, s.deps.now())
			if err != nil {
				s.exposureError(w, r, sess, cfg, "generating a certificate: "+err.Error())
				return
			}
			next.TLSCertFile, next.TLSKeyFile, fingerprint = certFile, keyFile, fp
		} else {
			// A tailnet has already encrypted the connection. Keeping a
			// certificate the operator chose earlier is fine; inventing one is
			// a second warning to click through for no gain.
			next.TLSCertFile, next.TLSKeyFile = cfg.Dashboard.TLSCertFile, cfg.Dashboard.TLSKeyFile
		}

	default:
		s.exposureError(w, r, sess, cfg, "Choose one of the three.")
		return
	}

	cfg.Dashboard = next
	if err := cfg.ValidateForUnit(nil, config.UnitScope{NoSecrets: true}); err != nil {
		s.exposureError(w, r, sess, cfg, "Nothing was written: "+err.Error())
		return
	}
	if err := setup.WriteConfig(s.deps.ConfigPath, cfg, true); err != nil {
		s.exposureError(w, r, sess, cfg, err.Error())
		return
	}
	s.deps.logger().Info("dashboard", "event", "exposure_changed", "exposure", string(exposure), "bind", next.Bind)

	s.render(w, http.StatusOK, "exposure", page{
		Title: "Access", Nav: "exposure", CSRF: sess.csrf, SignedIn: true,
		Flash: "Written. Restart kenward for it to take effect.",
		Data: exposureData{
			Cfg:         next,
			Current:     s.Addr(),
			Interfaces:  interfaceOptions(),
			Note:        privacy.DashboardNote(ReachFor(next), "https://"+next.Bind, next.TLS()),
			Fingerprint: fingerprint,
			Restart:     true,
		},
	})
}

func (s *Server) exposureError(w http.ResponseWriter, r *http.Request, sess *session, cfg *config.Config, msg string) {
	s.render(w, http.StatusBadRequest, "exposure", page{
		Title: "Access", Nav: "exposure", CSRF: sess.csrf, SignedIn: true,
		Error: msg,
		Data: exposureData{
			Cfg: cfg.Dashboard, Current: s.Addr(), Interfaces: interfaceOptions(),
			Note: privacy.DashboardNote(ReachFor(cfg.Dashboard), s.URL(), cfg.Dashboard.TLS()),
		},
	})
}

// interfaceOptions lists the addresses this machine has, so an operator picks one rather
// than typing it.
//
// Tailnet addresses are marked. Tailscale's range is 100.64.0.0/10 — the carrier-grade
// NAT block it deliberately squats — and an interface named tailscale0 or utun is the
// other half of the same guess. It is a hint on a radio button, not a security decision,
// so a wrong guess costs a wrong label and the operator can still see the address.
func interfaceOptions() []ifaceOption {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	var out []ifaceOption
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			opt := ifaceOption{Name: iface.Name, Addr: ipnet.IP.String()}
			switch {
			case cgnat != nil && cgnat.Contains(ipnet.IP),
				strings.HasPrefix(iface.Name, "tailscale"),
				strings.HasPrefix(iface.Name, "wg"):
				opt.Kind, opt.Tailnet = "looks like a tailnet or VPN", true
			case ipnet.IP.IsPrivate():
				opt.Kind = "your own network"
			default:
				opt.Kind = "reachable from outside this network"
			}
			out = append(out, opt)
		}
	}
	return out
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	_, port, _ := net.SplitHostPort(config.DefaultDashboardBind)
	return port
}

// ---------------------------------------------------------------- password

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request, sess *session) {
	created, _ := s.admin.CreatedAt()
	s.render(w, http.StatusOK, "password", page{
		Title: "Password", Nav: "password", CSRF: sess.csrf, SignedIn: true,
		Flash: flashOf(r),
		Data:  map[string]any{"Created": created, "Min": MinPasswordLength},
	})
}

// handlePasswordSubmit rotates the admin password, and signs every session out.
//
// Signing out afterwards is the point of changing it. Somebody who changes a password
// because they think it is known will not be helped by the other browser staying signed
// in — including, deliberately, this one.
func (s *Server) handlePasswordSubmit(w http.ResponseWriter, r *http.Request, sess *session) {
	old := r.PostFormValue("current")
	next := r.PostFormValue("password")
	if next != r.PostFormValue("password2") {
		s.render(w, http.StatusBadRequest, "password", page{
			Title: "Password", Nav: "password", CSRF: sess.csrf, SignedIn: true,
			Error: "The two new passwords are not the same.",
			Data:  map[string]any{"Min": MinPasswordLength},
		})
		return
	}
	if err := s.admin.ChangePassword(r.Context(), old, next); err != nil {
		msg := err.Error()
		if errors.Is(err, ErrBadPassword) {
			msg = "That is not the current password."
		}
		s.render(w, http.StatusUnauthorized, "password", page{
			Title: "Password", Nav: "password", CSRF: sess.csrf, SignedIn: true,
			Error: msg, Data: map[string]any{"Min": MinPasswordLength},
		})
		return
	}
	s.sess.destroyKind(kindAdmin)
	clearCookie(w, adminCookieName, s.secure())
	s.deps.logger().Info("dashboard", "event", "password_changed")
	redirectWith(w, r, "/login", "Password changed. Every browser has been signed out, including this one.")
}

// ---------------------------------------------------------------- helpers

// loadConfig reads kenward.yaml without insisting it is valid.
//
// Deliberately not config.Load: the dashboard's whole job is editing a configuration,
// and a configuration that has a problem in it is the one somebody most needs to open.
// Validation happens where it belongs — on the way out, before anything is written, and
// on the overview, where the problems are reported rather than fatal.
func (s *Server) loadConfig() (*config.Config, error) {
	f, err := os.Open(s.deps.ConfigPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg, err := config.Decode(f)
	if err != nil {
		return nil, err
	}
	if s.deps.DataDir != "" {
		cfg.DataDir = s.deps.DataDir
	}
	st, err := config.LoadState(cfg.StatePath())
	if err != nil {
		return nil, err
	}
	// A merge conflict — a telegram_id in the file disagreeing with the recorded
	// binding — is a problem to report, not a reason to refuse to show the file.
	_ = cfg.MergeState(st)
	return cfg, nil
}

// openLore builds a memory client for the request that needs one, and the caller closes
// it. A client holds an open SQLite store, and lore allows one per home per process, so
// one per request beats one held open for the life of a dashboard a household opens
// twice a year.
func (s *Server) openLore(r *http.Request) (SpaceClient, error) {
	if s.deps.Lore == nil {
		return nil, errors.New("this build has no way to reach lore")
	}
	return s.deps.Lore(r.Context())
}

// privacyModeFor maps a configured mode onto internal/privacy's own. It mirrors
// cmd/kenward's function of the same name, and both exist because internal/privacy must
// not depend on the shape of a configuration file to state what a topology protects.
func privacyModeFor(m config.Mode) privacy.Mode {
	if m == config.ModeIsolated {
		return privacy.ModeIsolated
	}
	return privacy.ModeSimple
}

// ReachFor maps a dashboard configuration onto internal/privacy's Reach.
//
// Exported because `kenward doctor` prints the same paragraph, and two mappings from a
// configuration onto a privacy claim is one mapping too many.
//
// It reads the declared exposure and not the address, because this is the claim being
// made to the household and config.validateDashboard has already refused any
// configuration where the claim and the socket disagree.
func ReachFor(d config.DashboardConfig) privacy.Reach {
	if !d.Enabled {
		return privacy.ReachOff
	}
	switch d.ExposureOrDefault() {
	case config.ExposureTailnet:
		return privacy.ReachTailnet
	case config.ExposureLAN:
		return privacy.ReachLAN
	default:
		return privacy.ReachLoopback
	}
}

// URLFor is the address to type into a browser for a configured dashboard, without a
// running server to ask. It is what `kenward doctor` names in its privacy paragraph.
//
// A wildcard bind is reported as loopback for the same reason Server.URL does it: 0.0.0.0
// is not an address anybody can visit, and printing it back at somebody who then pastes
// it into a browser helps nobody.
func URLFor(d config.DashboardConfig) string {
	scheme := "http"
	if d.TLS() {
		scheme = "https"
	}
	addr := d.BindAddr()
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsUnspecified() {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	return scheme + "://" + addr
}
