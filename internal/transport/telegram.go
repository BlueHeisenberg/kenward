package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Defaults for a Telegram transport. Every one of them is overridable with an
// Option; none of them requires configuration to be correct.
const (
	defaultPollTimeout      = time.Minute
	defaultUpdateBuffer     = 32
	defaultRateLimitRetries = 3
	defaultQuestionTimeout  = 5 * time.Minute
)

// Telegram is a Transport over the Telegram Bot API.
//
// It long-polls: the process makes outbound HTTPS requests and never listens on
// a port, so a household node needs no inbound firewall rule, no domain and no
// certificate. Webhooks are deliberately not supported.
//
// One Telegram is one bot token. In simple mode the household shares a token and
// a Mux fans the stream out; in isolated mode each pod holds its own.
//
// It is safe for concurrent use. Send and Ask may be called from any goroutine;
// Updates may be called once.
type Telegram struct {
	api   *bot.Bot
	token string // held only to scrub it out of errors; never logged, never returned

	maxLen         int
	retries        int
	retryDelay     func(retryAfter int) time.Duration
	updateBuf      int
	defaultTimeout time.Duration
	logger         *slog.Logger

	mu         sync.Mutex
	started    bool
	closed     bool
	closedCh   chan struct{}
	cancelPoll context.CancelFunc

	queue *queue
	wg    sync.WaitGroup

	// updatesGate is a test seam: when set, Updates calls it with t.mu still held,
	// at the instant before it is released, which is the instant the historical
	// Updates/Close WaitGroup race turned on. Nil in production.
	updatesGate func()

	pendingMu sync.Mutex
	pending   map[string]*pendingQuestion
}

// Option configures a Telegram transport.
type Option func(*telegramConfig)

type telegramConfig struct {
	serverURL      string
	httpClient     bot.HttpClient
	pollTimeout    time.Duration
	maxLen         int
	retries        int
	updateBuf      int
	queueCap       int
	defaultTimeout time.Duration
	logger         *slog.Logger
	skipTokenCheck bool
}

// WithAPIServer points the transport at a different Bot API root, for a
// self-hosted API server or a test double. Defaults to https://api.telegram.org.
func WithAPIServer(url string) Option {
	return func(c *telegramConfig) { c.serverURL = url }
}

// WithHTTPClient supplies the HTTP client used for every API call. The client's
// own timeout must exceed the poll timeout or long polling will churn.
func WithHTTPClient(client *http.Client) Option {
	return func(c *telegramConfig) { c.httpClient = client }
}

// WithPollTimeout sets how long a single getUpdates call may hang waiting for
// traffic. Longer is cheaper; the default is one minute.
func WithPollTimeout(d time.Duration) Option {
	return func(c *telegramConfig) { c.pollTimeout = d }
}

// WithMaxMessageLength overrides Telegram's 4096-unit message limit, which is
// useful only for exercising the splitter.
func WithMaxMessageLength(n int) Option {
	return func(c *telegramConfig) { c.maxLen = n }
}

// WithRateLimitRetries sets how many times a call is retried after a 429. Each
// retry waits exactly as long as Telegram's retry_after asks.
func WithRateLimitRetries(n int) Option {
	return func(c *telegramConfig) { c.retries = n }
}

// WithUpdateBuffer sets the capacity of the channel returned by Updates.
func WithUpdateBuffer(n int) Option {
	return func(c *telegramConfig) { c.updateBuf = n }
}

// WithBacklogLimit bounds the inbound messages held while the consumer is busy.
// Past the limit the oldest is dropped and counted rather than the process
// growing without end.
func WithBacklogLimit(n int) Option {
	return func(c *telegramConfig) { c.queueCap = n }
}

// WithQuestionTimeout sets the timeout applied to a Question that carries none.
func WithQuestionTimeout(d time.Duration) Option {
	return func(c *telegramConfig) { c.defaultTimeout = d }
}

// WithLogger attaches a logger. The transport logs delivery failures, drops and
// shutdown; it never logs message text, question text or the bot token.
func WithLogger(l *slog.Logger) Option {
	return func(c *telegramConfig) { c.logger = l }
}

// WithSkipTokenCheck skips the getMe call that NewTelegram otherwise makes to
// prove the token works. Useful offline; in production the check is what turns a
// bad token into a startup error instead of a silent nothing.
func WithSkipTokenCheck() Option {
	return func(c *telegramConfig) { c.skipTokenCheck = true }
}

// NewTelegram builds a transport for one bot token. Unless the check is skipped
// it verifies the token with Telegram before returning, so a misconfigured
// deployment fails at startup rather than at the first message.
func NewTelegram(token string, opts ...Option) (*Telegram, error) {
	cfg := telegramConfig{
		pollTimeout:    defaultPollTimeout,
		maxLen:         defaultMaxMessageLen,
		retries:        defaultRateLimitRetries,
		updateBuf:      defaultUpdateBuffer,
		queueCap:       defaultQueueCap,
		defaultTimeout: defaultQuestionTimeout,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("transport: empty bot token")
	}
	if cfg.pollTimeout < 2*time.Second {
		cfg.pollTimeout = 2 * time.Second
	}
	if cfg.maxLen <= 0 {
		cfg.maxLen = defaultMaxMessageLen
	}
	if cfg.updateBuf < 0 {
		cfg.updateBuf = 0
	}

	t := &Telegram{
		token:          token,
		maxLen:         cfg.maxLen,
		retries:        cfg.retries,
		retryDelay:     retryAfterDelay,
		updateBuf:      cfg.updateBuf,
		defaultTimeout: cfg.defaultTimeout,
		logger:         cfg.logger,
		closedCh:       make(chan struct{}),
		queue:          newQueue(cfg.queueCap),
		pending:        make(map[string]*pendingQuestion),
	}

	client := cfg.httpClient
	if client == nil {
		client = &http.Client{Timeout: cfg.pollTimeout + 10*time.Second}
	}

	bopts := []bot.Option{
		bot.WithDefaultHandler(t.onUpdate),
		bot.WithNotAsyncHandlers(),
		bot.WithErrorsHandler(t.onLibraryError),
		bot.WithHTTPClient(cfg.pollTimeout, client),
		bot.WithAllowedUpdates(bot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateCallbackQuery,
		}),
	}
	if cfg.serverURL != "" {
		bopts = append(bopts, bot.WithServerURL(cfg.serverURL))
	}
	if cfg.skipTokenCheck {
		bopts = append(bopts, bot.WithSkipGetMe())
	}

	api, err := bot.New(token, bopts...)
	if err != nil {
		return nil, fmt.Errorf("transport: telegram: %w", redactToken(err, token))
	}
	t.api = api
	return t, nil
}

// Updates starts long polling and returns the inbound stream.
//
// Only text messages from private chats, groups and supergroups are delivered.
// Photos, stickers, edits, channel posts, service messages and anything sent by
// another bot are ignored — quietly, because ignoring them is normal traffic and
// not an error.
//
// The channel is closed when ctx is done or the transport is closed. Updates may
// be called once; a second call returns ErrUpdatesActive.
func (t *Telegram) Updates(ctx context.Context) (<-chan Inbound, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	if t.started {
		t.mu.Unlock()
		return nil, ErrUpdatesActive
	}
	t.started = true
	pollCtx, cancel := context.WithCancel(ctx)
	t.cancelPoll = cancel

	out := make(chan Inbound, t.updateBuf)

	// The pumps are registered and launched while the lock is still held, for
	// the same reason muxView.Updates holds the Mux lock across its wg.Add:
	// Close marks the transport closed under this lock and only then calls
	// wg.Wait, so it either turns this call away at the top or waits for both
	// pumps. With the lock released first, a Close landing in the gap would see
	// a zero counter and return while the pumps start behind its back, waited
	// on by nobody.
	t.wg.Add(2)
	go func() {
		defer t.wg.Done()
		t.api.Start(pollCtx) // returns when pollCtx is done
		t.queue.close()
	}()
	go func() {
		defer t.wg.Done()
		defer close(out)
		for {
			in, ok := t.queue.pop()
			if !ok {
				return
			}
			select {
			case out <- in:
			case <-pollCtx.Done():
				return
			}
		}
	}()
	t.releaseStarted()

	return out, nil
}

// releaseStarted releases the lock Updates took, letting the test seam look
// first.
//
// The seam fires here, under the lock, rather than after it: the invariant is
// about this exact instant, because whoever takes t.mu next — Close, above all —
// must already find both pumps counted. A seam on the far side of the unlock
// cannot tell the two orderings apart, since by then wg.Add(2) has run either
// way. Call and unlock live in one function so that moving the unlock moves the
// observation with it.
func (t *Telegram) releaseStarted() {
	if t.updatesGate != nil {
		t.updatesGate()
	}
	t.mu.Unlock()
}

// Send delivers a reply, splitting it across several messages if it exceeds
// Telegram's length limit. ReplyTo applies to the first piece only.
//
// A failure part-way through is returned as an error naming the piece; the
// pieces already delivered stay delivered, because Telegram offers no way to
// take them back.
func (t *Telegram) Send(ctx context.Context, o Outbound) error {
	if err := t.state(); err != nil {
		return err
	}
	if o.ChatID == 0 {
		return errors.New("transport: send without a chat id")
	}
	if strings.TrimSpace(o.Text) == "" {
		return ErrEmptyText
	}

	parts := splitMessage(o.Text, t.maxLen)
	for i, part := range parts {
		replyTo := 0
		if i == 0 {
			replyTo = o.ReplyTo
		}
		if _, err := t.sendText(ctx, o.ChatID, part, replyTo, nil); err != nil {
			return fmt.Errorf("transport: send part %d of %d: %w", i+1, len(parts), err)
		}
	}
	return nil
}

// Ask puts a question with buttons in the chat and blocks until the allowed user
// taps one, the question times out, or ctx is done.
//
// Taps from anyone else are ignored: no acknowledgement, no state change, no
// trace. In a group chat every member sees the keyboard, and without that filter
// one member could decide where another member's memory is stored.
//
// A timeout returns Answer{TimedOut: true} and is a decline, never an accept.
// Either way the message is edited to drop the keyboard and show what happened,
// so nobody can tap an outcome that has already been decided.
//
// Taps arrive on the update stream, so Updates must be running for a question to
// be answerable; without it every question times out.
func (t *Telegram) Ask(ctx context.Context, q Question) (Answer, error) {
	if err := t.state(); err != nil {
		return Answer{}, err
	}
	if q.ChatID == 0 {
		return Answer{}, errors.New("transport: question without a chat id")
	}
	if strings.TrimSpace(q.Text) == "" {
		return Answer{}, ErrEmptyText
	}
	if len(q.Choices) == 0 {
		return Answer{}, ErrNoChoices
	}
	if utf16Len(q.Text)+retireReserve(q) > t.maxLen {
		return Answer{}, ErrTextTooLong
	}

	timeout := q.Timeout
	if timeout <= 0 {
		timeout = t.defaultTimeout
	}

	token, err := newQuestionToken()
	if err != nil {
		return Answer{}, fmt.Errorf("transport: ask: %w", err)
	}
	p := &pendingQuestion{
		allowed: q.AllowedUserID,
		choices: append([]Choice(nil), q.Choices...),
		done:    make(chan tap, 1),
	}
	t.pendingMu.Lock()
	t.pending[token] = p
	t.pendingMu.Unlock()
	defer t.forget(token)

	msg, err := t.sendText(ctx, q.ChatID, q.Text, 0, keyboardFor(token, q.Choices))
	if err != nil {
		return Answer{}, fmt.Errorf("transport: ask: %w", err)
	}
	if q.Posted != nil {
		q.Posted(msg.ID)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-p.done:
		return t.settle(ctx, q, msg.ID, p, r), nil

	case <-timer.C:
		// Close the door before retiring the message. If a tap won the race by a
		// hair it is already in the channel, and honouring it beats discarding a
		// decision the member has made.
		if !p.deliver(tap{}) {
			return t.settle(ctx, q, msg.ID, p, <-p.done), nil
		}
		t.retire(ctx, q.ChatID, msg.ID, retiredText(q, declinedText))
		return Answer{TimedOut: true}, nil

	case <-t.closedCh:
		p.deliver(tap{})
		// The transport is closing and Close does not wait for this cleanup, so
		// bound the edit rather than letting it run on the caller's live context
		// for up to the HTTP client timeout after Close has returned.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		t.retire(rctx, q.ChatID, msg.ID, retiredText(q, withdrawnText))
		rcancel()
		return Answer{}, ErrClosed

	case <-ctx.Done():
		p.deliver(tap{})
		t.retire(ctx, q.ChatID, msg.ID, retiredText(q, withdrawnText))
		return Answer{}, ctx.Err()
	}
}

// Close stops polling and releases the transport. It is idempotent; afterwards
// Updates, Send and Ask return ErrClosed and any Ask still waiting unblocks with
// ErrClosed.
func (t *Telegram) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.closedCh)
	cancel := t.cancelPoll
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	t.wg.Wait()

	if dropped := t.queue.dropped(); dropped > 0 {
		t.log(slog.LevelWarn, "inbound messages dropped", "count", dropped)
	}
	return nil
}

// --- update handling -------------------------------------------------------

func (t *Telegram) onUpdate(_ context.Context, _ *bot.Bot, upd *models.Update) {
	switch {
	case upd == nil:
		return
	case upd.CallbackQuery != nil:
		t.onCallback(upd.CallbackQuery)
	case upd.Message != nil:
		t.onMessage(upd.Message)
	default:
		// Edits, service messages, reactions: not a conversation turn.
	}
}

func (t *Telegram) onMessage(m *models.Message) {
	if m.From == nil || m.From.IsBot || m.Text == "" {
		return // non-text and machine traffic are ignored, not errors
	}
	var group bool
	switch m.Chat.Type {
	case models.ChatTypePrivate:
	case models.ChatTypeGroup, models.ChatTypeSupergroup:
		group = true
	default:
		return // channels are broadcast, not conversation
	}

	at := time.Now().UTC()
	if m.Date > 0 {
		at = time.Unix(int64(m.Date), 0).UTC()
	}

	if t.queue.push(Inbound{
		ChatID:    m.Chat.ID,
		UserID:    m.From.ID,
		Text:      m.Text,
		MessageID: m.ID,
		IsGroup:   group,
		At:        at,
	}) {
		t.log(slog.LevelWarn, "inbound backlog full, oldest message dropped", "chat_id", m.Chat.ID)
	}
}

// onCallback routes a button tap to the Ask that is waiting for it.
//
// Every rejection here is silent by design. A stale token, an unknown question, a
// tap from the wrong member: none of them gets a callback answer, because an
// answer is itself a signal, and the member who tapped is not owed one.
func (t *Telegram) onCallback(cq *models.CallbackQuery) {
	token, idx, ok := parseCallbackData(cq.Data)
	if !ok {
		return
	}
	if cq.From.ID == 0 {
		// A callback with no sender (never sent by real Telegram, reachable via
		// a buggy or hostile API server) is not a member's tap and must never
		// match a question, whatever its AllowedUserID.
		return
	}

	t.pendingMu.Lock()
	p := t.pending[token]
	t.pendingMu.Unlock()
	if p == nil {
		return // already answered, timed out, or from a previous run
	}
	if cq.From.ID != p.allowed {
		t.log(slog.LevelInfo, "callback ignored: not the addressed member", "user_id", cq.From.ID)
		return
	}
	if idx < 0 || idx >= len(p.choices) {
		return
	}

	p.deliver(tap{
		answer:     Answer{ChoiceID: p.choices[idx].ID, UserID: cq.From.ID},
		callbackID: cq.ID,
	})
}

func (t *Telegram) onLibraryError(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	t.log(slog.LevelWarn, "telegram api", "err", redactToken(err, t.token))
}

// --- questions -------------------------------------------------------------

// pendingQuestion is one outstanding Ask. The once is the race guard: exactly one
// outcome — a tap, a timeout, a cancellation — ever wins.
type pendingQuestion struct {
	allowed int64
	choices []Choice
	once    sync.Once
	done    chan tap
}

// tap is a decided outcome on its way back to Ask. A zero tap means the question
// was closed without an answer.
type tap struct {
	answer     Answer
	callbackID string
}

// deliver reports whether this outcome was the one that won.
func (p *pendingQuestion) deliver(r tap) bool {
	won := false
	p.once.Do(func() {
		p.done <- r
		won = true
	})
	return won
}

func (t *Telegram) forget(token string) {
	t.pendingMu.Lock()
	delete(t.pending, token)
	t.pendingMu.Unlock()
}

// settle acknowledges the tap, retires the message and returns the answer.
func (t *Telegram) settle(ctx context.Context, q Question, msgID int, p *pendingQuestion, r tap) Answer {
	if r.callbackID == "" {
		// Closed without a real tap; treated as a decline.
		t.retire(ctx, q.ChatID, msgID, retiredText(q, declinedText))
		return Answer{TimedOut: true}
	}
	t.ack(ctx, r.callbackID)
	t.retire(ctx, q.ChatID, msgID, answeredText(q, labelFor(p.choices, r.answer.ChoiceID)))
	return r.answer
}

// ack stops the spinner on the member's client. Best effort: a failure here costs
// them an animation, nothing more.
func (t *Telegram) ack(ctx context.Context, callbackID string) {
	err := t.call(ctx, func(ctx context.Context) error {
		_, err := t.api.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: callbackID})
		return err
	})
	if err != nil {
		t.log(slog.LevelDebug, "callback acknowledgement failed", "err", redactToken(err, t.token))
	}
}

// retire rewrites the question to show its outcome and strips the keyboard, so a
// decision cannot be tapped twice.
//
// Best effort, and safe if it fails: the pending question is gone from the map by
// then, so a tap on a keyboard left behind is ignored like any other stale tap.
func (t *Telegram) retire(ctx context.Context, chatID int64, msgID int, text string) {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
	}
	edit := func(text string, mode models.ParseMode) error {
		return t.call(ctx, func(ctx context.Context) error {
			_, err := t.api.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:      chatID,
				MessageID:   msgID,
				Text:        text,
				ParseMode:   mode,
				ReplyMarkup: emptyKeyboard(),
			})
			return err
		})
	}
	err := edit(text, parseMode)
	if err != nil && t.sendDegraded(ctx, "question outcome", err) {
		// Losing the edit loses the keyboard removal too, and a keyboard that
		// still looks tappable on a decision already made is the one thing
		// retiring exists to prevent. Unstyled is fine; not retired is not.
		err = edit(PlainText(text), "")
	}
	if err != nil {
		t.log(slog.LevelWarn, "could not retire question keyboard", "chat_id", chatID, "err", redactToken(err, t.token))
	}
}

// RetireKeyboard strips the buttons from a question this process did not ask.
//
// Ask retires its own message on every ending it can see — a tap, a timeout, a
// cancelled context, a transport closing — which covers everything except the ending
// it cannot see: the node being killed while the question is on screen. The keyboard
// outlives the process and the token behind it does not, so the next start finds a
// message that still looks live and answers nothing at all when it is tapped. This is
// how a caller that wrote the message id down clears it afterwards.
//
// A message whose keyboard is already gone — the run that died retired it and was
// killed before it could say so — comes back "message is not modified", which is an
// error saying nothing needed doing. The caller treats a failure here as tidying that
// did not happen, never as a reason to withhold what it was about to say.
//
// The text is left alone. The id is the only thing worth carrying across a restart,
// and rewriting a question this process never composed would mean keeping its copy in
// a state file; the caller says what it left the member on in a message of its own,
// which it knows and this package does not.
func (t *Telegram) RetireKeyboard(ctx context.Context, chatID int64, messageID int) error {
	if err := t.state(); err != nil {
		return err
	}
	if chatID == 0 || messageID == 0 {
		return errors.New("transport: retire without a message")
	}
	return t.call(ctx, func(ctx context.Context) error {
		_, err := t.api.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
			ChatID:      chatID,
			MessageID:   messageID,
			ReplyMarkup: emptyKeyboard(),
		})
		return err
	})
}

// keyboardFor lays the choices out one per row, so a long label is never clipped
// and the order the caller chose is the order the member reads.
//
// Callback data is "<token>:<index>", never the choice id: it stays inside
// Telegram's 64-byte budget whatever the caller names its choices, and the token
// ties the tap to one specific question rather than to a kind of question.
func keyboardFor(token string, choices []Choice) *models.InlineKeyboardMarkup {
	rows := make([][]models.InlineKeyboardButton, 0, len(choices))
	for i, c := range choices {
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         c.Label,
			CallbackData: token + ":" + strconv.Itoa(i),
		}})
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// emptyKeyboard is what removes a keyboard on edit: explicit, rather than relying
// on an omitted field.
func emptyKeyboard() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
}

func parseCallbackData(data string) (token string, idx int, ok bool) {
	sep := strings.LastIndex(data, ":")
	if sep <= 0 || sep == len(data)-1 {
		return "", 0, false
	}
	i, err := strconv.Atoi(data[sep+1:])
	if err != nil {
		return "", 0, false
	}
	return data[:sep], i, true
}

func newQuestionToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("question token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func labelFor(choices []Choice, id string) string {
	for _, c := range choices {
		if c.ID == id {
			return c.Label
		}
	}
	return id
}

// The outcome line appended to a spent question. Italic, because it is the node
// reporting on a button rather than more of what the question said — the same
// mark the retrieval line carries, for the same reason.
//
// The dash comes from the question's own language rather than from a literal here:
// it is the one piece of punctuation in this file that is not the same character in
// every language kenward speaks.
func outcomeText(q Question, phrase string) string {
	return q.Text + "\n\n" + Italic(q.Notes.orDefault().Dash+phrase)
}

func answeredText(q Question, label string) string { return outcomeText(q, label) }

func declinedText(q Question) string  { return outcomeText(q, q.Notes.orDefault().Declined) }
func withdrawnText(q Question) string { return outcomeText(q, q.Notes.orDefault().Withdrawn) }

// retiredText is the outcome line for a question that ended without a tap. def
// supplies the default wording for this particular ending, and Question.
// RetiredNote overrides every ending with one line — see its documentation for
// why the two are not distinguished when a caller sets it.
func retiredText(q Question, def func(Question) string) string {
	if q.RetiredNote != "" {
		return answeredText(q, q.RetiredNote)
	}
	return def(q)
}

// retireReserve is the room a question's text must leave for the outcome line
// appended when the message is retired — whichever outcome that turns out to
// be, including the longest choice label and the caller's own retired note.
// Computed from the outcome functions themselves so the reservation cannot drift
// from what retire actually writes.
//
// Every input to it is language-dependent, which is why it measures rather than
// assumes: the outcome phrases, the dash, the retired note and every button label
// are this conversation's, and French and Italian bodies run 15 to 25 per cent
// longer than English while Dutch has the longest button in the set. A fixed margin
// tuned on English would be silently wrong in half of them, and the failure is a
// 400 from Telegram on a long question rather than anything visible in a test.
//
// utf16Len is the right unit and not an approximation of one. Telegram counts UTF-16
// code units, so a Chinese character is one against the budget where it is three
// bytes, and an emoji is two where it is one rune. Counting bytes would over-reserve
// for every non-Latin script; counting runes would under-reserve for the glyphs.
func retireReserve(q Question) int {
	blank := Question{Notes: q.Notes}
	n := utf16Len(declinedText(blank))
	if w := utf16Len(withdrawnText(blank)); w > n {
		n = w
	}
	if q.RetiredNote != "" {
		if w := utf16Len(answeredText(blank, q.RetiredNote)); w > n {
			n = w
		}
	}
	for _, c := range q.Choices {
		if w := utf16Len(answeredText(blank, c.Label)); w > n {
			n = w
		}
	}
	return n
}

// --- api plumbing ----------------------------------------------------------

// sendText delivers one already-formatted piece.
//
// Text is Telegram HTML — see format.go — and is sent with the parse mode set.
// If Telegram rejects it as malformed the same words go out again as plain text,
// because a member losing a memory confirmation to a stray angle bracket is a far
// worse failure than one reading it unstyled. See sendDegraded.
func (t *Telegram) sendText(ctx context.Context, chatID int64, text string, replyTo int, kb *models.InlineKeyboardMarkup) (*models.Message, error) {
	params := &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: parseMode,
	}
	if replyTo != 0 {
		params.ReplyParameters = &models.ReplyParameters{
			MessageID:                replyTo,
			AllowSendingWithoutReply: true,
		}
	}
	if kb != nil {
		params.ReplyMarkup = kb
	}

	send := func(ctx context.Context) (*models.Message, error) {
		var msg *models.Message
		err := t.call(ctx, func(ctx context.Context) error {
			m, err := t.api.SendMessage(ctx, params)
			if err != nil {
				return err
			}
			msg = m
			return nil
		})
		return msg, err
	}

	msg, err := send(ctx)
	if err != nil && t.sendDegraded(ctx, "message", err) {
		params.Text = PlainText(text)
		params.ParseMode = ""
		msg, err = send(ctx)
		if err != nil {
			// Report the formatting failure, not the second one: the first is
			// what went wrong, and the retry only failed because the send was
			// never going to work.
			return nil, redactToken(err, t.token)
		}
	}
	if err != nil {
		return nil, redactToken(err, t.token)
	}
	if msg == nil {
		msg = &models.Message{}
	}
	return msg, nil
}

// sendDegraded reports whether err is worth retrying without formatting, and
// logs it when it is.
//
// Any 400 qualifies. Telegram signals a formatting fault in prose — "can't parse
// entities: Unsupported start tag" — and matching on that prose would make
// kenward's delivery guarantee depend on Telegram's choice of words. A 400 for
// some other reason costs one wasted call on a path that was failing anyway, and
// the error the caller sees is still the real one.
func (t *Telegram) sendDegraded(ctx context.Context, what string, err error) bool {
	if ctx.Err() != nil || !errors.Is(err, bot.ErrorBadRequest) {
		return false
	}
	t.log(slog.LevelWarn, "telegram rejected a formatted "+what+", resending it unformatted",
		"err", redactToken(err, t.token))
	return true
}

// call runs an API call, honouring Telegram's 429 back-pressure: it waits exactly
// the retry_after Telegram asks for, up to a bounded number of attempts, and
// gives up early if ctx is done. Any other error is returned as it is.
func (t *Telegram) call(ctx context.Context, fn func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}

		var limited *bot.TooManyRequestsError
		if !errors.As(err, &limited) || attempt >= t.retries {
			return err
		}

		wait := t.retryDelay(limited.RetryAfter)
		t.log(slog.LevelWarn, "rate limited by telegram", "retry_after_s", limited.RetryAfter, "attempt", attempt+1)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-t.closedCh:
			timer.Stop()
			return ErrClosed
		}
	}
}

func retryAfterDelay(retryAfter int) time.Duration {
	if retryAfter <= 0 {
		retryAfter = 1
	}
	return time.Duration(retryAfter) * time.Second
}

func (t *Telegram) state() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrClosed
	}
	return nil
}

func (t *Telegram) log(level slog.Level, msg string, args ...any) {
	if t.logger == nil {
		return
	}
	t.logger.Log(context.Background(), level, msg, args...)
}

// redactedError hides a bot token that leaked into an error string while keeping
// errors.Is and errors.As working on what it wraps.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactToken scrubs the bot token out of an error, in its plain form and in
// the URL-encoded forms an HTTP error could plausibly carry. The token is a
// credential that must never reach a log line, a refusal or a bug report, and
// this scrub is the only thing standing between the two.
func redactToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	redacted := msg
	for _, form := range []string{token, url.QueryEscape(token), url.PathEscape(token)} {
		redacted = strings.ReplaceAll(redacted, form, "«bot token»")
	}
	if redacted == msg {
		return err
	}
	return &redactedError{msg: redacted, err: err}
}

// Telegram implements Transport.
var _ Transport = (*Telegram)(nil)
