package cmd

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// fakeRoundTripper is a test-only http.RoundTripper that returns a preset response/error.
type fakeRoundTripper struct {
	resp *http.Response
	err  error
}

func (f *fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return f.resp, f.err
}

func fakeReq(path string) *http.Request {
	req, _ := http.NewRequest("GET", "https://api.telegram.org/bot123"+path, nil)
	return req
}

// TestPollStampRoundTripper_SuccessOnlyStamp verifies that:
//   - A getUpdates 200 response advances lastSuccess.
//   - A getUpdates error does NOT advance lastSuccess but sets lastErr.
//   - A getUpdates 500 does NOT advance lastSuccess but sets lastErr.
//   - A non-getUpdates 200 does NOT touch lastSuccess.
func TestPollStampRoundTripper_SuccessOnlyStamp(t *testing.T) {
	epoch := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := epoch

	watch := &pollWatchdog{}

	rt := &pollStampRoundTripper{
		watch: watch,
		now:   func() time.Time { return clock },
	}

	// Case 1: getUpdates 200 — must advance lastSuccess.
	clock = epoch.Add(1 * time.Second)
	rt.base = &fakeRoundTripper{resp: &http.Response{StatusCode: 200}}
	rt.RoundTrip(fakeReq("/getUpdates"))
	ls, _, le := watch.snapshot()
	if !ls.Equal(clock) {
		t.Errorf("expected lastSuccess=%v after 200, got %v", clock, ls)
	}
	if le != "" {
		t.Errorf("expected empty lastErr after 200, got %q", le)
	}
	successAfterOK := ls

	// Case 2: getUpdates transport error — must NOT advance lastSuccess.
	clock = epoch.Add(10 * time.Second)
	rt.base = &fakeRoundTripper{err: errors.New("connection reset")}
	rt.RoundTrip(fakeReq("/getUpdates"))
	ls, _, le = watch.snapshot()
	if !ls.Equal(successAfterOK) {
		t.Errorf("lastSuccess must not advance on error; got %v, want %v", ls, successAfterOK)
	}
	if le == "" {
		t.Errorf("expected lastErr to be set after transport error")
	}

	// Case 3: getUpdates 500 — must NOT advance lastSuccess.
	clock = epoch.Add(20 * time.Second)
	rt.base = &fakeRoundTripper{resp: &http.Response{StatusCode: 500}}
	rt.RoundTrip(fakeReq("/getUpdates"))
	ls, _, le = watch.snapshot()
	if !ls.Equal(successAfterOK) {
		t.Errorf("lastSuccess must not advance on 500; got %v, want %v", ls, successAfterOK)
	}
	if le == "" {
		t.Errorf("expected lastErr to be set after 500")
	}

	// Case 4: non-getUpdates 200 (e.g. sendMessage) — must NOT touch lastSuccess.
	clock = epoch.Add(30 * time.Second)
	rt.base = &fakeRoundTripper{resp: &http.Response{StatusCode: 200}}
	rt.RoundTrip(fakeReq("/sendMessage"))
	ls2, _, _ := watch.snapshot()
	if !ls2.Equal(successAfterOK) {
		t.Errorf("non-getUpdates 200 must not advance lastSuccess; got %v, want %v", ls2, successAfterOK)
	}
}

// TestPollWatchdog_StaleUnderFailures verifies that once the clock advances beyond
// lastSuccess + threshold, stale() returns true even when lastAttempt keeps advancing.
func TestPollWatchdog_StaleUnderFailures(t *testing.T) {
	epoch := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	watch := &pollWatchdog{}

	// Seed lastSuccess at epoch.
	watch.onResult(epoch, true, "")

	threshold := 60 * time.Second

	// At epoch+59s with continued failures — not yet stale.
	for i := 1; i <= 5; i++ {
		watch.onResult(epoch.Add(time.Duration(i)*10*time.Second), false, "timeout")
	}
	if watch.stale(epoch.Add(59*time.Second), threshold) {
		t.Errorf("stale should be false at epoch+59s")
	}

	// At epoch+61s — stale.
	watch.onResult(epoch.Add(61*time.Second), false, "timeout")
	if !watch.stale(epoch.Add(61*time.Second), threshold) {
		t.Errorf("stale should be true at epoch+61s")
	}

	// Confirm lastAttempt advanced while lastSuccess stayed at epoch.
	ls, la, _ := watch.snapshot()
	if !ls.Equal(epoch) {
		t.Errorf("lastSuccess must still equal seed epoch; got %v", ls)
	}
	if !la.After(ls) {
		t.Errorf("lastAttempt must have advanced beyond lastSuccess; got %v", la)
	}
}
