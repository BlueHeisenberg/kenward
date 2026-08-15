package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a stand-in for api.telegram.org.
//
// The go-telegram/bot library lets the API root be redirected, so the whole
// transport — long polling, multipart encoding, error decoding, 429 handling —
// runs unmodified against an httptest server. Nothing here reaches the network,
// and no test needs a bot token that exists.
type fakeAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	calls    []recordedCall
	pending  []json.RawMessage
	scripted map[string][]string
	nextMsg  int

	signal chan struct{}
}

// recordedCall is one API call the transport made, with its form fields decoded.
type recordedCall struct {
	Method string
	Form   url.Values
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		scripted: map[string][]string{},
		nextMsg:  1000,
		signal:   make(chan struct{}, 1),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) URL() string { return f.srv.URL }

// push queues a raw update for the next getUpdates call.
func (f *fakeAPI) push(raw string) {
	f.mu.Lock()
	f.pending = append(f.pending, json.RawMessage(raw))
	f.mu.Unlock()
	select {
	case f.signal <- struct{}{}:
	default:
	}
}

// script queues a canned response body for the next call to method, which is how
// a 429 or any other API failure is injected.
func (f *fakeAPI) script(method, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripted[method] = append(f.scripted[method], body)
}

func (f *fakeAPI) callsFor(method string) []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeAPI) countFor(method string) int { return len(f.callsFor(method)) }

// waitCall blocks until method has been called at least n times and returns the
// nth call, failing the test if it does not happen.
func (f *fakeAPI) waitCall(t *testing.T, method string, n int) recordedCall {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls := f.callsFor(method); len(calls) >= n {
			return calls[n-1]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for call %d to %s (saw %d)", n, method, f.countFor(method))
	return recordedCall{}
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	method := parts[len(parts)-1]

	form := url.Values{}
	if err := r.ParseMultipartForm(1 << 20); err == nil && r.MultipartForm != nil {
		for k, v := range r.MultipartForm.Value {
			form[k] = v
		}
	}

	if method != "getUpdates" {
		f.mu.Lock()
		f.calls = append(f.calls, recordedCall{Method: method, Form: form})
		f.mu.Unlock()
	}

	if body, ok := f.popScripted(method); ok {
		writeJSON(w, body)
		return
	}

	switch method {
	case "getMe":
		writeJSON(w, `{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"kenward","username":"kenward_bot"}}`)
	case "getUpdates":
		writeJSON(w, f.longPoll(r))
	case "sendMessage", "editMessageText":
		f.mu.Lock()
		f.nextMsg++
		id := f.nextMsg
		f.mu.Unlock()
		chatID := form.Get("chat_id")
		if chatID == "" {
			chatID = "0"
		}
		writeJSON(w, fmt.Sprintf(
			`{"ok":true,"result":{"message_id":%d,"date":1700000000,"chat":{"id":%s,"type":"private"}}}`,
			id, chatID))
	default:
		writeJSON(w, `{"ok":true,"result":true}`)
	}
}

// longPoll mimics Telegram's long polling: it holds the request briefly rather
// than answering an empty batch instantly, so the transport does not spin.
func (f *fakeAPI) longPoll(r *http.Request) string {
	deadline := time.After(100 * time.Millisecond)
	for {
		f.mu.Lock()
		if len(f.pending) > 0 {
			batch := f.pending
			f.pending = nil
			f.mu.Unlock()
			parts := make([]string, 0, len(batch))
			for _, b := range batch {
				parts = append(parts, string(b))
			}
			return `{"ok":true,"result":[` + strings.Join(parts, ",") + `]}`
		}
		f.mu.Unlock()

		select {
		case <-f.signal:
		case <-deadline:
			return `{"ok":true,"result":[]}`
		case <-r.Context().Done():
			return `{"ok":true,"result":[]}`
		}
	}
}

func (f *fakeAPI) popScripted(method string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	queued := f.scripted[method]
	if len(queued) == 0 {
		return "", false
	}
	f.scripted[method] = queued[1:]
	return queued[0], true
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// --- update fixtures -------------------------------------------------------

var updateSeq int64

func nextUpdateID() int64 {
	updateSeq++
	return updateSeq
}

// textUpdate builds a plain text message update. chatType is "private", "group",
// "supergroup" or "channel".
func textUpdate(chatID, userID int64, chatType, text string) string {
	return fmt.Sprintf(
		`{"update_id":%d,"message":{"message_id":%d,"date":1700000000,"from":{"id":%d,"is_bot":false,"first_name":"M"},"chat":{"id":%d,"type":"%s"},"text":%q}}`,
		nextUpdateID(), nextUpdateID()+500, userID, chatID, chatType, text)
}

// botTextUpdate is a message sent by another bot, which must be ignored.
func botTextUpdate(chatID, userID int64, text string) string {
	return fmt.Sprintf(
		`{"update_id":%d,"message":{"message_id":%d,"date":1700000000,"from":{"id":%d,"is_bot":true,"first_name":"B"},"chat":{"id":%d,"type":"private"},"text":%q}}`,
		nextUpdateID(), nextUpdateID()+500, userID, chatID, text)
}

// photoUpdate is a message with no text, which must be ignored cleanly.
func photoUpdate(chatID, userID int64) string {
	return fmt.Sprintf(
		`{"update_id":%d,"message":{"message_id":%d,"date":1700000000,"from":{"id":%d,"is_bot":false,"first_name":"M"},"chat":{"id":%d,"type":"private"},"photo":[{"file_id":"x","file_unique_id":"y","width":1,"height":1}]}}`,
		nextUpdateID(), nextUpdateID()+500, userID, chatID)
}

// channelPostUpdate is a broadcast, not a conversation.
func channelPostUpdate(chatID int64, text string) string {
	return fmt.Sprintf(
		`{"update_id":%d,"channel_post":{"message_id":%d,"date":1700000000,"chat":{"id":%d,"type":"channel"},"text":%q}}`,
		nextUpdateID(), nextUpdateID()+500, chatID, text)
}

// callbackUpdate is a button tap.
func callbackUpdate(cbID string, userID, chatID int64, msgID int, data string) string {
	return fmt.Sprintf(
		`{"update_id":%d,"callback_query":{"id":%q,"chat_instance":"ci","from":{"id":%d,"is_bot":false,"first_name":"M"},"data":%q,"message":{"message_id":%d,"date":1700000000,"chat":{"id":%d,"type":"private"}}}}`,
		nextUpdateID(), cbID, userID, data, msgID, chatID)
}

// --- assertions ------------------------------------------------------------

// keyboardFromForm decodes the reply_markup of a recorded call.
func keyboardFromForm(t *testing.T, c recordedCall) [][]struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
} {
	t.Helper()
	var kb struct {
		InlineKeyboard [][]struct {
			Text         string `json:"text"`
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
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

// assertNoGoroutineLeak fails if the goroutine count has not returned to base.
// It retries, because a goroutine that is on its way out has not always left by
// the time the test asks.
func assertNoGoroutineLeak(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.Gosched()
		now := runtime.NumGoroutine()
		if now <= base {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak: %d at start, %d now\n%s", base, now, buf[:n])
		}
		time.Sleep(5 * time.Millisecond)
	}
}
