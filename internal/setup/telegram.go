package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TelegramTimeout bounds one getMe.
//
// Ten seconds, and not the three ProbeTimeout gives an endpoint: this is a real HTTPS
// round trip to api.telegram.org over whatever connection the household has, made once,
// while somebody watches. A short timeout here would report a working bot as unreachable
// on a slow link, which is the one answer that would send an operator off to debug
// something that is not wrong.
const TelegramTimeout = 10 * time.Second

// BotInfo is what Telegram says about a token, from getMe.
type BotInfo struct {
	// Username is the bot the token belongs to, without the leading @. It is the one
	// thing that tells somebody they are pointed at the bot they meant rather than at
	// last month's test bot.
	Username string
	// ReadsGroupMessages is getMe's can_read_all_group_messages, and it is the whole
	// reason this probe reports more than a name.
	//
	// Telegram turns bot privacy mode ON for every new bot. With it on, a bot in a
	// group receives nothing at all — not plain messages, not even an @mention; only
	// `/start@thebot` is delivered. So a household adds the bot to their family group
	// and it ignores everyone, with no error, no warning and no log line anywhere,
	// because nothing ever arrives to log. It is fixed in BotFather with `/setprivacy`
	// → the bot → Disable, and Telegram applies the change only to groups the bot
	// joins afterwards, so a bot already in the group has to be removed and re-added.
	ReadsGroupMessages bool
}

// TelegramProbe asks Telegram about a bot token. It is a function type for the same
// reason Probe and ModelsProbe are: the wizard, the dashboard and `kenward doctor` all
// have to be testable against a token Telegram accepts, one it refuses, and a Telegram
// that is not reachable, and none of those can be arranged with the real one.
type TelegramProbe func(ctx context.Context, token string) (BotInfo, error)

// TelegramAPIBase is the Bot API root. It is a variable so a test can point the
// production probe at a local server; nothing else changes it.
var TelegramAPIBase = "https://api.telegram.org"

// DefaultTelegramProbe authorises a bot token and reports what Telegram says about it.
//
// It is the only getMe anyone asks before there is a transport. `kenward doctor` calls
// it through this type, and so do both wizards, because "which bot is this and can it
// hear the group" is one question and two answers to it would eventually differ. The
// running transport makes its own, once, at startup — it has to, because it needs its
// own username to recognise an @mention and by then no wizard is in the picture.
//
// The token goes in the URL path, which is where the Bot API wants it, and is scrubbed
// out of every error before it can reach a terminal or a log: net/url and net/http both
// quote the whole URL in their errors.
func DefaultTelegramProbe(ctx context.Context, token string) (BotInfo, error) {
	if strings.TrimSpace(token) == "" {
		return BotInfo{}, errors.New("the bot token variable is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, TelegramTimeout)
	defer cancel()

	endpoint := TelegramAPIBase + "/bot" + url.PathEscape(token) + "/getMe"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return BotInfo{}, ScrubToken(err, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return BotInfo{}, ScrubToken(err, token)
	}
	defer resp.Body.Close()

	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Username string `json:"username"`
			// Absent on an old Bot API, which decodes as false and produces the
			// advice rather than silence. That is the right way round: the advice
			// costs a paragraph and the silence costs a household that is ignored.
			CanReadAllGroupMessages bool `json:"can_read_all_group_messages"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return BotInfo{}, fmt.Errorf("telegram answered %s with something that is not a getMe response", resp.Status)
	}
	if !body.OK {
		if body.Description != "" {
			return BotInfo{}, fmt.Errorf("telegram refused the token: %s", body.Description)
		}
		return BotInfo{}, fmt.Errorf("telegram refused the token (%s)", resp.Status)
	}
	return BotInfo{
		Username:           body.Result.Username,
		ReadsGroupMessages: body.Result.CanReadAllGroupMessages,
	}, nil
}

// ScrubToken removes a bot token from an error before it is shown to anyone.
func ScrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), token, "<bot token>")
	msg = strings.ReplaceAll(msg, url.PathEscape(token), "<bot token>")
	return errors.New(msg)
}
