// Package tgfake is a scripted fake of the Telegram Bot API for unit tests: an httptest server that
// answers per method and keeps an ordered log of every request.
package tgfake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tele "gopkg.in/telebot.v3"
)

// Reply is one scripted answer. Err set means a Telegram error: HTTP status 400 and the
// body {"ok":false,"error_code":400,"description":Err}; otherwise {"ok":true,"result":Result}.
type Reply struct {
	Result interface{}
	Err    string
}

// Call is one recorded request: the Bot API method and its decoded JSON body.
type Call struct {
	Method string
	Params map[string]interface{}
}

// Server is the fake Bot API.
type Server struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []Call
	script map[string]func(p map[string]interface{}) Reply
}

// New starts the fake server and closes it when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{script: map[string]func(map[string]interface{}) Reply{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	params := map[string]interface{}{}
	json.Unmarshal(body, &params)
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Params: params})
	f := s.script[method]
	s.mu.Unlock()
	reply := defaultReply(method)
	if f != nil {
		reply = f(params)
	}
	w.Header().Set("Content-Type", "application/json")
	if reply.Err != "" {
		w.WriteHeader(http.StatusBadRequest)
		out, _ := json.Marshal(map[string]interface{}{"ok": false, "error_code": http.StatusBadRequest, "description": reply.Err})
		w.Write(out)
		return
	}
	out, _ := json.Marshal(map[string]interface{}{"ok": true, "result": reply.Result})
	w.Write(out)
}

// defaultReply answers a method nobody scripted: a message result for the send/edit family, true otherwise.
func defaultReply(method string) Reply {
	switch method {
	case "sendMessage", "editMessageText", "sendRichMessage":
		return Reply{Result: map[string]interface{}{"message_id": 9001, "chat": map[string]interface{}{"id": 1}}}
	}
	return Reply{Result: true}
}

// On scripts the answer of a method. The function runs outside the server lock, so it may call back into the test.
func (s *Server) On(method string, f func(p map[string]interface{}) Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script[method] = f
}

// Bot returns an Offline, Synchronous telebot bot wired to the fake server.
func (s *Server) Bot(t *testing.T) *tele.Bot {
	t.Helper()
	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: s.srv.URL, Offline: true, Synchronous: true, Client: s.srv.Client()})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot
}

// Calls returns a snapshot of the recorded requests in arrival order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Count returns how many requests used method.
func (s *Server) Count(method string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

// Methods returns the recorded method names in arrival order (for ordering assertions).
func (s *Server) Methods() []string {
	var out []string
	for _, c := range s.Calls() {
		out = append(out, c.Method)
	}
	return out
}
