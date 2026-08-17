// Package telegramtest is a local Telegram Bot API server for tests.
//
// The go-telegram/bot library lets the API root be redirected, and
// transport.WithAPIServer exposes that, so the whole transport — long polling,
// multipart encoding, response decoding, error decoding, 429 handling, inline
// keyboards and callback queries — runs unmodified against an httptest server.
// Nothing reaches the network and no test needs a bot token that exists.
//
// It lives in its own package rather than in a _test.go file because two suites
// need it: the transport's own tests, which drive one Telegram directly, and the
// end-to-end suite, which drives a whole household over one. A second
// hand-written stand-in would be a second thing to keep honest.
//
// What "honest" means here is mostly getUpdates. Telegram does not consume an
// update when it hands it over: it keeps it until a later call asks for a higher
// offset, and hands it out again in the meantime. A server that instead drained
// its queue on read would answer identically for a correct client and for one
// that never advanced its offset at all — which is precisely the bug worth
// catching, so this one keeps the log and confirms by offset.
package telegramtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// pollHold is how long a getUpdates call with nothing to say waits before
// answering an empty batch. Real Telegram is told a timeout in seconds by the
// client; this is short enough to keep a test quick and long enough that the
// transport does not spin.
const pollHold = 100 * time.Millisecond

// Server is a stand-in for api.telegram.org, listening on loopback.
//
// It is safe for concurrent use: the HTTP handler and the test goroutine share
// every field behind one mutex.
type Server struct {
	srv   *httptest.Server
	token string

	mu        sync.Mutex
	calls     []Call
	offsets   []int64
	pending   []pendingUpdate
	delivered map[int64]int
	nextID    int64
	nextMsg   int
	scripted  map[string][]scriptedReply
	holds     map[string]chan struct{}

	signal chan struct{}
}

// Call is one API call the client made, with its multipart form decoded.
type Call struct {
	Method string
	Form   url.Values
	// MessageID is the id the server gave the resulting Message, for sendMessage
	// and editMessageText. It is what a test needs to address a callback_query at
	// the keyboard a particular send drew, and to check that an edit retired that
	// same message rather than some other one.
	MessageID int
}

// pendingUpdate is one update the server holds until an offset confirms it.
type pendingUpdate struct {
	id  int64
	raw string
}

type scriptedReply struct {
	status int
	body   string
}

// New starts a server that answers only for token. Calls carrying any other
// token get Telegram's 401, which is what makes "the transport sent the token it
// was given" an assertion rather than an assumption.
func New(t *testing.T, token string) *Server {
	t.Helper()
	s := &Server{
		token:     token,
		delivered: map[int64]int{},
		scripted:  map[string][]scriptedReply{},
		holds:     map[string]chan struct{}{},
		nextMsg:   1000,
		signal:    make(chan struct{}, 1),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is what goes into transport.WithAPIServer.
func (s *Server) URL() string { return s.srv.URL }

// --- driving the server ----------------------------------------------------

// Push queues an update for delivery and returns the update_id it was given.
//
// body is an update object without its update_id: numbering belongs to the
// server, because an offset only means anything against ids the server chose.
func (s *Server) Push(body string) int64 {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "{") {
		panic("telegramtest: update body must be a JSON object")
	}

	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.pending = append(s.pending, pendingUpdate{
		id:  id,
		raw: fmt.Sprintf(`{"update_id":%d,%s`, id, strings.TrimPrefix(body, "{")),
	})
	s.mu.Unlock()

	select {
	case s.signal <- struct{}{}:
	default:
	}
	return id
}

// Script queues a canned 200 response for the next call to method, which is how
// a 429 or any other API-level failure is injected.
func (s *Server) Script(method, body string) { s.ScriptStatus(method, http.StatusOK, body) }

// ScriptStatus is Script with the HTTP status too, for the failures that are not
// a well-formed API error at all — a gateway's 502, an HTML error page.
func (s *Server) ScriptStatus(method string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripted[method] = append(s.scripted[method], scriptedReply{status: status, body: body})
}

// Hold makes every call to method hang until the returned function is called or
// the request's own context ends, which is how a slow or wedged API is injected.
func (s *Server) Hold(method string) (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.holds[method] = gate
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// --- reading the server ----------------------------------------------------

// CallsFor returns every recorded call to method, in order. getUpdates is not
// recorded here — it is polled constantly and would bury everything else; see
// Offsets and Deliveries for what it did.
func (s *Server) CallsFor(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// CountFor reports how many times method was called.
func (s *Server) CountFor(method string) int { return len(s.CallsFor(method)) }

// WaitCall blocks until method has been called at least n times and returns the
// nth call, failing the test if it does not happen.
func (s *Server) WaitCall(t *testing.T, method string, n int) Call {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls := s.CallsFor(method); len(calls) >= n {
			return calls[n-1]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for call %d to %s (saw %d)", n, method, s.CountFor(method))
	return Call{}
}

// Pending reports how many pushed updates are still unconfirmed. Zero after a
// message has been handled means the client really did advance its offset;
// non-zero means it never acknowledged what it was given.
func (s *Server) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// Deliveries reports how many times one update was handed to the client. More
// than one is a redelivery: legitimate after a failed poll, a bug otherwise.
func (s *Server) Deliveries(id int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered[id]
}

// Offsets returns the offset every getUpdates call asked for, in order,
// including the calls that were answered with a failure.
func (s *Server) Offsets() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, len(s.offsets))
	copy(out, s.offsets)
	return out
}

// --- the wire --------------------------------------------------------------

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// The library builds "<root>/bot<token>/<method>".
	prefix, method, ok := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
	if !ok || strings.TrimPrefix(prefix, "bot") != s.token {
		writeJSON(w, http.StatusUnauthorized,
			`{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}

	form := url.Values{}
	if err := r.ParseMultipartForm(1 << 20); err == nil && r.MultipartForm != nil {
		for k, v := range r.MultipartForm.Value {
			form[k] = v
		}
	}

	// The offset is recorded on arrival, before the hold and before any scripted
	// reply, so a poll answered with a failure is still counted. That is the point
	// of counting them: a client that asks for the same offset again after a
	// failure has correctly confirmed nothing, and that is only visible if the
	// failed call was seen.
	//
	// Everything else is recorded on the way out instead, once the client has its
	// answer — see served below.
	if method == "getUpdates" {
		offset, _ := strconv.ParseInt(form.Get("offset"), 10, 64)
		s.mu.Lock()
		s.offsets = append(s.offsets, offset)
		s.mu.Unlock()
	}

	// A call becomes visible to a test only once it has been answered.
	//
	// CallsFor and WaitCall are how a test says "the client has made this call",
	// and what it does next is built on that: it cancels a context, it reads the
	// MessageID the server handed back, it pushes a callback_query at the keyboard
	// a particular send drew. None of that is sound while the request is still in
	// flight. Recording on arrival made both of those a race the fake itself
	// manufactured — a cancellation that beat the send it had just waited for, and
	// a MessageID read as the zero the field is filled in from — and both showed up
	// as CI failures on the loaded runners and on no developer's machine.
	//
	// A nil served is a call that was never answered: the client gave up during a
	// hold, and a call the client got nothing back from is not one it made.
	var served *Call
	if method != "getUpdates" {
		defer func() {
			if served == nil {
				return
			}
			s.mu.Lock()
			s.calls = append(s.calls, *served)
			s.mu.Unlock()
		}()
	}

	s.mu.Lock()
	gate := s.holds[method]
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return // the client gave up; answer nothing
		}
	}
	served = &Call{Method: method, Form: form}

	if reply, ok := s.popScripted(method); ok {
		writeJSON(w, reply.status, reply.body)
		return
	}

	switch method {
	case "getMe":
		writeJSON(w, http.StatusOK, fmt.Sprintf(
			`{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"kenward","username":%q}}`,
			BotUsername))

	case "getUpdates":
		offset, _ := strconv.ParseInt(form.Get("offset"), 10, 64)
		writeJSON(w, http.StatusOK, s.poll(r, offset))

	case "sendMessage", "editMessageText":
		body, id := s.messageResult(form)
		served.MessageID = id
		writeJSON(w, http.StatusOK, body)

	case "answerCallbackQuery":
		writeJSON(w, http.StatusOK, `{"ok":true,"result":true}`)

	default:
		// Deliberately not a permissive "ok". A method this server does not model
		// is a gap in the server, and it should say so where the test can see it
		// rather than answer something plausible.
		writeJSON(w, http.StatusNotFound,
			`{"ok":false,"error_code":404,"description":"Not Found: method not found"}`)
	}
}

// poll answers one getUpdates call with Telegram's own offset semantics: the
// offset confirms everything below it, and whatever is left is handed over —
// again, if it was handed over before and never confirmed.
func (s *Server) poll(r *http.Request, offset int64) string {
	s.confirm(offset)

	deadline := time.After(pollHold)
	for {
		if batch := s.take(); batch != "" {
			return batch
		}
		select {
		case <-s.signal:
		case <-deadline:
			return `{"ok":true,"result":[]}`
		case <-r.Context().Done():
			return `{"ok":true,"result":[]}`
		}
	}
}

// confirm forgets every update the client acknowledged by asking for a higher
// offset. Nothing else forgets an update: that is the whole point.
func (s *Server) confirm(offset int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]pendingUpdate, 0, len(s.pending))
	for _, u := range s.pending {
		if u.id >= offset {
			kept = append(kept, u)
		}
	}
	s.pending = kept
}

// take returns every unconfirmed update as one batch, or "" if there are none.
// The updates stay in the log; only a later offset removes them.
func (s *Server) take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.pending))
	for _, u := range s.pending {
		s.delivered[u.id]++
		parts = append(parts, u.raw)
	}
	return `{"ok":true,"result":[` + strings.Join(parts, ",") + `]}`
}

// messageResult echoes back the Message Telegram would have created, and the id
// it gave it. An edit keeps the id it edited; a send gets a fresh one.
func (s *Server) messageResult(form url.Values) (string, int) {
	chatID, _ := strconv.ParseInt(form.Get("chat_id"), 10, 64)
	chatType := "private"
	if chatID < 0 {
		chatType = "supergroup"
	}

	id, err := strconv.Atoi(form.Get("message_id"))
	if err != nil {
		s.mu.Lock()
		s.nextMsg++
		id = s.nextMsg
		s.mu.Unlock()
	}

	result, err := json.Marshal(map[string]any{
		"message_id": id,
		"date":       1700000000,
		"chat":       map[string]any{"id": chatID, "type": chatType},
		"text":       form.Get("text"),
	})
	if err != nil {
		return `{"ok":false,"error_code":500,"description":"Internal Server Error"}`, id
	}
	return `{"ok":true,"result":` + string(result) + `}`, id
}

func (s *Server) popScripted(method string) (scriptedReply, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := s.scripted[method]
	if len(queued) == 0 {
		return scriptedReply{}, false
	}
	s.scripted[method] = queued[1:]
	return queued[0], true
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// --- update fixtures -------------------------------------------------------
//
// Each returns one update object without its update_id, ready for Push.

// TextUpdate is a plain text message. chatType is "private", "group",
// "supergroup" or "channel".
func TextUpdate(chatID, userID int64, chatType, text string) string {
	return fmt.Sprintf(
		`{"message":{"message_id":%d,"date":1700000000,"from":{"id":%d,"is_bot":false,"first_name":"M"},"chat":{"id":%d,"type":%q},"text":%q}}`,
		messageID(), userID, chatID, chatType, text)
}

// BotUsername is the bot getMe answers for on this server. A test that wants a
// message addressed to it names it through MentionUpdate rather than spelling the
// handle out, so the two cannot drift apart.
const BotUsername = "kenward_bot"

// MentionUpdate is a supergroup message that @mentions this server's bot, carrying
// the mention entity Telegram sends with it.
//
// The entity is the point. Telegram does not mark a message as addressed; it sends
// an offset and a length, in UTF-16 code units, and leaves the bot to work out
// whether the handle at that offset is its own. A test that pushed the text without
// the entity would be pushing a message Telegram never sends.
func MentionUpdate(chatID, userID int64, text string) string {
	handle := "@" + BotUsername
	before, _, ok := strings.Cut(text, handle)
	if !ok {
		panic("telegramtest: MentionUpdate text does not mention " + handle)
	}
	entity := fmt.Sprintf(`{"type":"mention","offset":%d,"length":%d}`,
		len(utf16.Encode([]rune(before))), len(utf16.Encode([]rune(handle))))
	return fmt.Sprintf(
		`{"message":{"message_id":%d,"date":1700000000,"from":{"id":%d,"is_bot":false,"first_name":"M"},"chat":{"id":%d,"type":"supergroup"},"text":%q,"entities":[%s]}}`,
		messageID(), userID, chatID, text, entity)
}

// BotTextUpdate is a message sent by another bot, which must be ignored.
func BotTextUpdate(chatID, userID int64, text string) string {
	return fmt.Sprintf(
		`{"message":{"message_id":%d,"date":1700000000,"from":{"id":%d,"is_bot":true,"first_name":"B"},"chat":{"id":%d,"type":"private"},"text":%q}}`,
		messageID(), userID, chatID, text)
}

// PhotoUpdate is a message with no text, which must be ignored cleanly.
func PhotoUpdate(chatID, userID int64) string {
	return fmt.Sprintf(
		`{"message":{"message_id":%d,"date":1700000000,"from":{"id":%d,"is_bot":false,"first_name":"M"},"chat":{"id":%d,"type":"private"},"photo":[{"file_id":"x","file_unique_id":"y","width":1,"height":1}]}}`,
		messageID(), userID, chatID)
}

// ChannelPostUpdate is a broadcast, not a conversation.
func ChannelPostUpdate(chatID int64, text string) string {
	return fmt.Sprintf(
		`{"channel_post":{"message_id":%d,"date":1700000000,"chat":{"id":%d,"type":"channel"},"text":%q}}`,
		messageID(), chatID, text)
}

// CallbackUpdate is a button tap. data is the callback_data the keyboard
// carried, which a test reads back off the sendMessage that drew it.
func CallbackUpdate(cbID string, userID, chatID int64, msgID int, data string) string {
	return fmt.Sprintf(
		`{"callback_query":{"id":%q,"chat_instance":"ci","from":{"id":%d,"is_bot":false,"first_name":"M"},"data":%q,"message":{"message_id":%d,"date":1700000000,"chat":{"id":%d,"type":"private"}}}}`,
		cbID, userID, data, msgID, chatID)
}

// CallbackUpdateNoFrom is a malformed tap with no sender, which real Telegram
// never produces but a buggy or hostile API server could.
func CallbackUpdateNoFrom(cbID string, chatID int64, msgID int, data string) string {
	return fmt.Sprintf(
		`{"callback_query":{"id":%q,"chat_instance":"ci","data":%q,"message":{"message_id":%d,"date":1700000000,"chat":{"id":%d,"type":"private"}}}}`,
		cbID, data, msgID, chatID)
}

// messageSeq hands out message ids that are unique within a process. They are
// not update ids — the server owns those — and nothing asserts on them; they
// exist because two messages sharing an id would be a confusing fixture.
var messageSeq struct {
	mu sync.Mutex
	n  int
}

func messageID() int {
	messageSeq.mu.Lock()
	defer messageSeq.mu.Unlock()
	messageSeq.n++
	return 500 + messageSeq.n
}

// --- assertions ------------------------------------------------------------

// Button is one inline keyboard button as it arrived on the wire.
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// Keyboard decodes the reply_markup of a recorded call, failing the test if
// there was none.
func Keyboard(t *testing.T, c Call) [][]Button {
	t.Helper()
	var kb struct {
		InlineKeyboard [][]Button `json:"inline_keyboard"`
	}
	raw := c.Form.Get("reply_markup")
	if raw == "" {
		t.Fatalf("call to %s carried no reply_markup", c.Method)
	}
	if err := json.Unmarshal([]byte(raw), &kb); err != nil {
		t.Fatalf("decode reply_markup: %v", err)
	}
	return kb.InlineKeyboard
}
