package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	tele "gopkg.in/telebot.v3"
)

// apiCall records one raw HTTP request the fake Bot API server received: the Bot API method name
// parsed from the request path (telebot always POSTs to ".../bot<token>/<method>", v3.3.8 api.go:23)
// and the raw JSON body, plus the message_id this call targeted (empty for methods with no
// message_id, e.g. answerCallbackQuery).
type apiCall struct {
	method string
	body   string
	msgID  string
}

// trackingCalls is a mutex-guarded recorder: these tests inspect calls made by a
// handleAudioTranscription goroutine that is STILL RUNNING concurrently with the test (to check an
// in-flight queued/running status before releasing a gate), so every read must be synchronized with
// the fake server's own append to stay go test -race clean. nextMsgID assigns an INCREMENTING
// message_id per sendMessage (S5) so two messages in flight at once (e.g. two concurrent
// handleAudioTranscription calls) can be told apart by id instead of by recording position.
type trackingCalls struct {
	mu        sync.Mutex
	calls     []apiCall
	nextMsgID int
}

func (tc *trackingCalls) record(c apiCall) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.calls = append(tc.calls, c)
}

func (tc *trackingCalls) nextMessageID() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.nextMsgID++
	return strconv.Itoa(tc.nextMsgID)
}

func (tc *trackingCalls) snapshot() []apiCall {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	out := make([]apiCall, len(tc.calls))
	copy(out, tc.calls)
	return out
}

// waitFor polls (always under the mutex, never reading tc.calls unsynchronized) until at least n calls
// have been recorded, returning a snapshot; fails the test on timeout.
func (tc *trackingCalls) waitFor(t *testing.T, n int, timeout time.Duration) []apiCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if snap := tc.snapshot(); len(snap) >= n {
			return snap
		}
		if time.Now().After(deadline) {
			snap := tc.snapshot()
			t.Fatalf("timed out waiting for %d recorded calls, have %d: %+v", n, len(snap), snap)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// parseParams decodes a recorded call's raw JSON body into its flat string-keyed param map. Safe to
// call from any goroutine (no *testing.T dependency) — used internally by the fake server (which runs
// its handler in its own goroutine per request) to read message_id out of incoming
// editMessageText/answerCallbackQuery request bodies.
func parseParams(body string) map[string]string {
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil
	}
	return m
}

// decodeCallParams is parseParams for use in test assertions (always called from the test's own
// goroutine, so t.Fatalf here is safe), failing the test if the body doesn't decode. telebot's
// embedSendOptions marshals every param, including reply_markup, into a JSON STRING value
// (options.go:190-194, v3.3.8: replyMarkup, _ := json.Marshal(...); params["reply_markup"] =
// string(replyMarkup), and params is a map[string]string) — so the whole raw body is itself a flat
// map[string]string, never a nested object. Decoding it lets exactness assertions (SPEC TC4/TC5/
// TC6/TC8/TC8c say EXACTLY) compare field values with ==, which a substring check on raw JSON cannot
// prove.
func decodeCallParams(t *testing.T, body string) map[string]string {
	t.Helper()
	m := parseParams(body)
	if m == nil {
		t.Fatalf("failed to decode call body as a flat string map: %q", body)
	}
	return m
}

// newTrackingBot returns an Offline telebot bot wired to a fake Bot API server. sendMessage gets an
// INCREMENTING message_id (9001, 9002, ...); editMessageText's message_id is read back out of the
// request body itself (the id it's targeting); every other method (answerCallbackQuery, etc.) is
// recorded with no message_id and gets Telegram's real result shape for that family of call.
func newTrackingBot(t *testing.T) (*tele.Bot, *trackingCalls) {
	t.Helper()
	tc := &trackingCalls{nextMsgID: 9000}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "sendMessage":
			id := tc.nextMessageID()
			tc.record(apiCall{method: method, body: string(b), msgID: id})
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%s,"chat":{"id":1}}}`, id)
		case "editMessageText":
			id := parseParams(string(b))["message_id"]
			tc.record(apiCall{method: method, body: string(b), msgID: id})
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%s,"chat":{"id":1}}}`, id)
		default:
			tc.record(apiCall{method: method, body: string(b)})
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: srv.URL, Offline: true, Synchronous: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot, tc
}

// fakeClock is the statusNowFn seam's test double: a mutex-guarded, test-advanceable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeTicker is the statusTickerFn seam's test double. Each call creates its own channel (one per
// handleAudioTranscription call's background ticker goroutine) and removes it from the active set
// when that goroutine's stop func runs.
type fakeTicker struct {
	mu    sync.Mutex
	chans map[int]chan time.Time
	seq   int
}

func newFakeTicker() *fakeTicker { return &fakeTicker{chans: map[int]chan time.Time{}} }

func (f *fakeTicker) seam() (<-chan time.Time, func()) {
	f.mu.Lock()
	id := f.seq
	f.seq++
	ch := make(chan time.Time)
	f.chans[id] = ch
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.chans, id)
		f.mu.Unlock()
	}
}

// fire (replaces the round-1 fireAll — S4) waits, bounded, until EXACTLY `alive` ticker channels are
// currently registered, then sends `at` to each. fireAll sent only to whatever happened to already be
// registered, so a tick could be dispatched before the owning handler goroutine's statusTickerFn()
// call had registered its channel yet — the tick silently vanishes and the test passes vacuously
// (whatever update() does or doesn't do, since it's simply never invoked). Waiting for the exact count
// makes that race impossible to pass through unnoticed; it also lets a test positively assert how many
// tickers are currently alive (e.g. TC5 uses alive=1 to prove call1's ticker already deregistered).
func (f *fakeTicker) fire(t *testing.T, alive int, at time.Time) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var chans []chan time.Time
	for {
		f.mu.Lock()
		n := len(f.chans)
		if n == alive {
			chans = make([]chan time.Time, 0, n)
			for _, ch := range f.chans {
				chans = append(chans, ch)
			}
			f.mu.Unlock()
			break
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d live ticker(s), have %d", alive, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, ch := range chans {
		ch <- at
	}
}

// assertAudioQueueEmpty (S3) fails the test immediately if any ticket is still registered in the
// package-global FIFO queue. The queue is shared across every test in this package, so a ticket
// leaked by an earlier test (a t.Fatalf fired while a ticket was held, before its audioQueueLeave
// ran) would otherwise surface as a confusing failure in a LATER, unrelated test (a wrong ahead count,
// or audioQueueWaitTurn blocking until go test's own 10-minute timeout) — calling this at the start of
// every queue-touching test turns a leak into an immediate, clearly-attributed failure instead.
func assertAudioQueueEmpty(t *testing.T) {
	t.Helper()
	audioQueueMu.Lock()
	n := len(audioQueueList)
	audioQueueMu.Unlock()
	if n != 0 {
		t.Fatalf("audio queue is not empty at test start: %d ticket(s) still registered (leaked by an earlier test)", n)
	}
}

// pinStatusClock (R2) pins statusNowFn to a fixed instant for the test's duration (restored via
// t.Cleanup). audioStatusText rounds elapsed time with Round(time.Second), so without pinning,
// >=500ms of real wall-clock time between audioQueueEnter and the post-WaitTurn updateAudioStatus
// call would produce an extra "...1s" edit and break every exact call-count assertion that runs on
// the real clock. With the clock fixed, the real 10s ticker also can't produce a changed-text edit,
// so it can never add one either.
func pinStatusClock(t *testing.T) {
	t.Helper()
	orig := statusNowFn
	t.Cleanup(func() { statusNowFn = orig })
	fixed := time.Unix(1700000000, 0)
	statusNowFn = func() time.Time { return fixed }
}

// TestAudioQueue_EnterOrderAndLeave covers SPEC TC2: three goroutines enter the FIFO queue in a
// strictly test-controlled order (each gated by a channel so audioQueueEnter cannot race ahead of the
// previous one), asserting ahead is 0, 1, 2 respectively; audioQueueLeave on the first ticket drops
// audioQueueAhead for the third ticket from 2 to 1.
func TestAudioQueue_EnterOrderAndLeave(t *testing.T) {
	assertAudioQueueEmpty(t)
	type entry struct {
		id    int64
		ahead int
	}
	gate1 := make(chan struct{})
	gate2 := make(chan struct{})
	results := make(chan entry, 3)

	go func() {
		id, ahead := audioQueueEnter()
		t.Cleanup(func() { audioQueueLeave(id) })
		results <- entry{id, ahead}
		close(gate1)
	}()
	<-gate1
	go func() {
		id, ahead := audioQueueEnter()
		t.Cleanup(func() { audioQueueLeave(id) })
		results <- entry{id, ahead}
		close(gate2)
	}()
	<-gate2
	go func() {
		id, ahead := audioQueueEnter()
		t.Cleanup(func() { audioQueueLeave(id) })
		results <- entry{id, ahead}
	}()

	var got [3]entry
	for i := 0; i < 3; i++ {
		got[i] = <-results
	}
	if got[0].ahead != 0 {
		t.Errorf("1st ticket ahead = %d, want 0", got[0].ahead)
	}
	if got[1].ahead != 1 {
		t.Errorf("2nd ticket ahead = %d, want 1", got[1].ahead)
	}
	if got[2].ahead != 2 {
		t.Errorf("3rd ticket ahead = %d, want 2", got[2].ahead)
	}

	id3 := got[2].id
	if before := audioQueueAhead(id3); before != 2 {
		t.Fatalf("audioQueueAhead(id3) before any leave = %d, want 2", before)
	}
	audioQueueLeave(got[0].id)
	if after := audioQueueAhead(id3); after != 1 {
		t.Errorf("audioQueueAhead(id3) after 1st ticket left = %d, want 1", after)
	}
	audioQueueLeave(got[1].id)
	audioQueueLeave(id3)
}

// TestAudioQueue_WaitTurnBlocksUntilLeave covers SPEC TC3: audioQueueWaitTurn for the second-entered
// ticket blocks while the first-entered ticket is still head, and returns promptly (with a fresh
// becameHeadAt) once the first ticket leaves.
func TestAudioQueue_WaitTurnBlocksUntilLeave(t *testing.T) {
	assertAudioQueueEmpty(t)
	id1, _ := audioQueueEnter()
	t.Cleanup(func() { audioQueueLeave(id1) })
	id2, ahead2 := audioQueueEnter()
	t.Cleanup(func() { audioQueueLeave(id2) })
	if ahead2 != 1 {
		t.Fatalf("ahead2 = %d, want 1", ahead2)
	}

	resultCh := make(chan time.Time, 1)
	go func() { resultCh <- audioQueueWaitTurn(id2) }()

	select {
	case <-resultCh:
		t.Fatal("audioQueueWaitTurn(id2) returned before id1 left the queue, want it to block")
	case <-time.After(100 * time.Millisecond):
	}

	before := time.Now()
	audioQueueLeave(id1)

	select {
	case becameHeadAt := <-resultCh:
		after := time.Now()
		if becameHeadAt.Before(before) {
			t.Errorf("becameHeadAt = %v, want >= %v (set at the leave)", becameHeadAt, before)
		}
		if becameHeadAt.After(after) {
			t.Errorf("becameHeadAt = %v, want <= %v (receive time, tolerance of now)", becameHeadAt, after)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("audioQueueWaitTurn(id2) did not return after id1 left")
	}

	audioQueueLeave(id2)
}

// TestAudioStatusText_ReturnsNotOkAfterTicketLeaves: a tick firing for a ticket that has ALREADY left
// the queue (the window between audioQueueLeave, called the instant the engine call returns, and the
// ticker goroutine actually being stopped) must be a silent no-op — never fall back to "Queued (0
// ahead)", which SPEC.md says must never be shown once a ticket has started running.
func TestAudioStatusText_ReturnsNotOkAfterTicketLeaves(t *testing.T) {
	assertAudioQueueEmpty(t)
	id, ahead := audioQueueEnter()
	t.Cleanup(func() { audioQueueLeave(id) })
	if ahead != 0 {
		t.Fatalf("ahead = %d, want 0", ahead)
	}
	if text, ok := audioStatusText(id, "Transcribing", "whisper"); !ok || !strings.Contains(text, "Transcribing with Whisper") {
		t.Fatalf("audioStatusText while still queued = (%q, %v), want a running text and ok=true", text, ok)
	}

	audioQueueLeave(id)

	text, ok := audioStatusText(id, "Transcribing", "whisper")
	if ok {
		t.Errorf("audioStatusText after leave: ok = true, want false (ticket no longer registered)")
	}
	if text != "" {
		t.Errorf("audioStatusText after leave: text = %q, want empty", text)
	}

	bot, tc := newTrackingBot(t)
	status := newAudioStatus(bot, newAudioCtx(bot), "⏳ Transcribing with Whisper… 0s")
	updateAudioStatus(status, id, "Transcribing", "whisper")
	if calls := tc.snapshot(); len(calls) != 1 {
		t.Errorf("calls after updateAudioStatus on a left ticket = %d, want 1 (only the initial send above; updateAudioStatus must be a no-op), got %+v", len(calls), calls)
	}
}

func TestHandleAudioTranscription_InitialStatusShowsZeroSecondsRunning(t *testing.T) {
	assertAudioQueueEmpty(t)
	pinStatusClock(t)
	setupTestConfig(t, config.AppConfig{VoiceEngine: "whisper"})
	bot, tc := newTrackingBot(t)

	origDL, origTF, origPUI := downloadAudio, transcribeFn, processUserInputFn
	t.Cleanup(func() { downloadAudio, transcribeFn, processUserInputFn = origDL, origTF, origPUI })

	downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
	processUserInputFn = func(bs *types.BotState, c tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
		return nil
	}
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	transcribeFn = func(path string) (string, string, error) {
		<-gate
		return "hello", "whisper", nil
	}

	done := make(chan error, 1)
	go func() {
		done <- handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil)
	}()
	wait := sync.OnceValue(func() error { return <-done })
	t.Cleanup(func() {
		release()
		wait()
	})

	calls := tc.waitFor(t, 1, 2*time.Second)
	if calls[0].method != "sendMessage" {
		t.Fatalf("first call method = %q, want sendMessage", calls[0].method)
	}
	params := decodeCallParams(t, calls[0].body)
	if params["text"] != "⏳ Transcribing with Whisper… 0s" {
		t.Errorf("first call text = %q, want exactly the running-at-0s status", params["text"])
	}

	release()
	if err := wait(); err != nil {
		t.Fatalf("handleAudioTranscription: %v", err)
	}
	final := tc.waitFor(t, 2, 2*time.Second)
	if len(final) != 2 {
		t.Fatalf("total calls = %d, want exactly 2 (initial send + final edit)", len(final))
	}
	if final[1].method != "editMessageText" {
		t.Errorf("second call method = %q, want editMessageText", final[1].method)
	}
}

func TestHandleAudioTranscription_QueuePositionAndElapsedTiming(t *testing.T) {
	assertAudioQueueEmpty(t)
	setupTestConfig(t, config.AppConfig{VoiceEngine: "whisper"})
	bot, tc := newTrackingBot(t)

	origNow, origTicker, origDL, origTF, origPUI := statusNowFn, statusTickerFn, downloadAudio, transcribeFn, processUserInputFn
	t.Cleanup(func() {
		statusNowFn, statusTickerFn, downloadAudio, transcribeFn, processUserInputFn = origNow, origTicker, origDL, origTF, origPUI
	})

	clock := newFakeClock(time.Unix(1700000000, 0))
	statusNowFn = clock.Now
	ft := newFakeTicker()
	statusTickerFn = ft.seam

	downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, fileID), nil }
	processUserInputFn = func(bs *types.BotState, c tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
		return nil
	}
	gate1 := make(chan struct{})
	gate2 := make(chan struct{})
	release1 := sync.OnceFunc(func() { close(gate1) })
	release2 := sync.OnceFunc(func() { close(gate2) })
	transcribeFn = func(path string) (string, string, error) {
		content, _ := os.ReadFile(path)
		switch string(content) {
		case "file1":
			<-gate1
			return "first", "whisper", nil
		case "file2":
			<-gate2
			return "second", "whisper", nil
		}
		return "", "whisper", fmt.Errorf("unexpected content %q", content)
	}

	done1 := make(chan error, 1)
	done2 := make(chan error, 1)
	wait1 := sync.OnceValue(func() error { return <-done1 })
	wait2 := sync.OnceValue(func() error { return <-done2 })
	t.Cleanup(func() {
		release1()
		release2()
		wait1()
		wait2()
	})

	go func() {
		done1 <- handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil)
	}()
	calls := tc.waitFor(t, 1, 2*time.Second)
	call1MsgID := calls[0].msgID
	if p := decodeCallParams(t, calls[0].body); p["text"] != "⏳ Transcribing with Whisper… 0s" {
		t.Fatalf("1st call's initial send text = %q, want exactly the running-at-0s status", p["text"])
	}

	go func() {
		done2 <- handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file2", "ogg", "🗣️", "", nil)
	}()
	calls = tc.waitFor(t, 2, 2*time.Second)
	call2MsgID := calls[1].msgID
	if p := decodeCallParams(t, calls[1].body); p["text"] != "🕒 Queued (1 ahead)" {
		t.Fatalf("2nd call's initial send text = %q, want exactly Queued (1 ahead)", p["text"])
	}
	if call1MsgID == call2MsgID {
		t.Fatalf("call1 and call2 got the SAME message_id (%s) — the fake server must assign distinct ids", call1MsgID)
	}

	clock.Advance(30 * time.Second)
	release1()
	if err := wait1(); err != nil {
		t.Fatalf("1st handleAudioTranscription: %v", err)
	}

	// 2 more calls land here: call1's own final transcript edit and call2's flip-to-running edit
	// (woken by call1's audioQueueLeave) — located by message_id, not position: the relative order
	// between the two goroutines' edits is not guaranteed.
	calls = tc.waitFor(t, 4, 2*time.Second)
	var call1FinalText, call2FlipText string
	for _, c := range calls[2:4] {
		p := decodeCallParams(t, c.body)
		switch c.msgID {
		case call1MsgID:
			call1FinalText = p["text"]
		case call2MsgID:
			call2FlipText = p["text"]
		default:
			t.Fatalf("unexpected message_id %q in call %+v", c.msgID, c)
		}
	}
	if call1FinalText != "🗣️ first" {
		t.Errorf("call1's final transcript edit text = %q, want exactly %q", call1FinalText, "🗣️ first")
	}
	if call2FlipText != "⏳ Transcribing with Whisper… 0s" {
		t.Errorf("call2's flip-to-running edit text = %q, want exactly …0s (not the 30s it spent queued)", call2FlipText)
	}

	// alive=1 here also proves call1's ticker goroutine already deregistered (self-removed on stop()).
	clock.Advance(10 * time.Second)
	ft.fire(t, 1, clock.Now())
	calls = tc.waitFor(t, 5, 2*time.Second)
	if calls[4].msgID != call2MsgID {
		t.Fatalf("tick-driven edit targeted message_id %q, want call2's %q", calls[4].msgID, call2MsgID)
	}
	if p := decodeCallParams(t, calls[4].body); p["text"] != "⏳ Transcribing with Whisper… 10s" {
		t.Errorf("tick-driven edit text = %q, want exactly …10s", p["text"])
	}

	release2()
	if err := wait2(); err != nil {
		t.Fatalf("2nd handleAudioTranscription: %v", err)
	}
}

func TestHandleAudioTranscription_TickerSkipsUnchangedText(t *testing.T) {
	assertAudioQueueEmpty(t)
	setupTestConfig(t, config.AppConfig{VoiceEngine: "whisper"})
	bot, tc := newTrackingBot(t)

	origNow, origTicker, origDL, origTF, origPUI := statusNowFn, statusTickerFn, downloadAudio, transcribeFn, processUserInputFn
	t.Cleanup(func() {
		statusNowFn, statusTickerFn, downloadAudio, transcribeFn, processUserInputFn = origNow, origTicker, origDL, origTF, origPUI
	})

	clock := newFakeClock(time.Unix(1700000000, 0))
	statusNowFn = clock.Now
	ft := newFakeTicker()
	statusTickerFn = ft.seam

	downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
	processUserInputFn = func(bs *types.BotState, c tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
		return nil
	}
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	transcribeFn = func(path string) (string, string, error) { <-gate; return "hello", "whisper", nil }

	done := make(chan error, 1)
	go func() {
		done <- handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil)
	}()
	wait := sync.OnceValue(func() error { return <-done })
	t.Cleanup(func() {
		release()
		wait()
	})

	calls := tc.waitFor(t, 1, 2*time.Second)
	if p := decodeCallParams(t, calls[0].body); p["text"] != "⏳ Transcribing with Whisper… 0s" {
		t.Fatalf("initial send text = %q, want exactly the running-at-0s status", p["text"])
	}

	// Two ticks at the SAME unchanged clock value (replaces the round-1 time.Sleep(50ms) absence
	// check — S4): the ticker goroutine handles one tick at a time (single select loop) and
	// update()'s HTTP call is synchronous, so by the time the second (unbuffered) send has been
	// received, the first tick's updateAudioStatus call has already returned and the goroutine is
	// back at the select statement. No sleep needed to prove the first tick's no-op effect landed.
	ft.fire(t, 1, clock.Now())
	ft.fire(t, 1, clock.Now())
	if got := tc.snapshot(); len(got) != 1 {
		t.Fatalf("calls after two no-change ticks = %d, want still 1 (no-op), got %+v", len(got), got)
	}

	clock.Advance(10 * time.Second)
	ft.fire(t, 1, clock.Now())
	tc.waitFor(t, 2, 2*time.Second)
	// Two more ticks at the SAME clock value (now 10s): each send on the unbuffered tick channel returns only after the
	// ticker goroutine finished the previous tick's synchronous update(), so once the second one is received every
	// Bot API call the 10s tick made (including a duplicate edit) is already recorded.
	ft.fire(t, 1, clock.Now())
	ft.fire(t, 1, clock.Now())
	calls = tc.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls after the 10s tick = %d, want exactly 2 (initial send + one edit), got %+v", len(calls), calls)
	}
	if calls[1].method != "editMessageText" {
		t.Errorf("tick-driven call method = %q, want editMessageText", calls[1].method)
	}
	if calls[1].msgID != calls[0].msgID {
		t.Errorf("tick-driven edit targets msg_id %q, want the initial send's %q", calls[1].msgID, calls[0].msgID)
	}
	if p := decodeCallParams(t, calls[1].body); p["text"] != "⏳ Transcribing with Whisper… 10s" {
		t.Errorf("tick-driven edit text = %q, want exactly %q", p["text"], "⏳ Transcribing with Whisper… 10s")
	}

	release()
	if err := wait(); err != nil {
		t.Fatalf("handleAudioTranscription: %v", err)
	}
}

func TestHandleAudioTranscription_QueueLeavesBeforeRoutingCompletes(t *testing.T) {
	assertAudioQueueEmpty(t)
	pinStatusClock(t)
	setupTestConfig(t, config.AppConfig{VoiceEngine: "whisper"})
	bot, tc := newTrackingBot(t)

	origDL, origTF, origPUI := downloadAudio, transcribeFn, processUserInputFn
	t.Cleanup(func() { downloadAudio, transcribeFn, processUserInputFn = origDL, origTF, origPUI })

	downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
	transcribeFn = func(path string) (string, string, error) { return "transcript", "whisper", nil }
	routingGate := make(chan struct{})
	releaseRouting := sync.OnceFunc(func() { close(routingGate) })
	processUserInputFn = func(bs *types.BotState, c tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
		<-routingGate
		return nil
	}

	done1 := make(chan error, 1)
	done2 := make(chan error, 1)
	wait1 := sync.OnceValue(func() error { return <-done1 })
	wait2 := sync.OnceValue(func() error { return <-done2 })
	t.Cleanup(func() {
		releaseRouting()
		wait1()
		wait2()
	})

	go func() {
		done1 <- handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil)
	}()
	// 2 recorded calls = the initial send + the final transcript edit — proof the engine call has
	// already returned (audioQueueLeave already ran) and processUserInputFn has now been entered (and
	// is blocked on routingGate).
	tc.waitFor(t, 2, 2*time.Second)

	go func() {
		done2 <- handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file2", "ogg", "🗣️", "", nil)
	}()
	calls := tc.waitFor(t, 3, 2*time.Second)
	if p := decodeCallParams(t, calls[2].body); p["text"] != "⏳ Transcribing with Whisper… 0s" {
		t.Errorf("2nd call's initial send text = %q, want exactly the running-at-0s status (queue already released by call1's engine return, not blocked on call1's still-running processUserInputFn)", p["text"])
	}

	releaseRouting()
	if err := wait1(); err != nil {
		t.Fatalf("1st handleAudioTranscription: %v", err)
	}
	if err := wait2(); err != nil {
		t.Fatalf("2nd handleAudioTranscription: %v", err)
	}
}
