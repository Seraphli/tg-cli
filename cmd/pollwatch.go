package cmd

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type pollWatchdog struct {
	mu          sync.Mutex
	lastSuccess time.Time
	lastAttempt time.Time
	lastErr     string
}

func (w *pollWatchdog) onResult(now time.Time, ok bool, errStr string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.After(w.lastAttempt) {
		w.lastAttempt = now
	}
	if ok {
		if now.After(w.lastSuccess) {
			w.lastSuccess = now
		}
		w.lastErr = ""
	} else if errStr != "" {
		w.lastErr = errStr
	}
}

func (w *pollWatchdog) stale(now time.Time, threshold time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return now.Sub(w.lastSuccess) > threshold
}

func (w *pollWatchdog) snapshot() (lastSuccess, lastAttempt time.Time, lastErr string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastSuccess, w.lastAttempt, w.lastErr
}

// pollStampRoundTripper wraps an http.RoundTripper and stamps the pollWatchdog
// on each getUpdates call. Only a real 2xx response advances lastSuccess; errors
// and non-2xx responses record lastErr but never advance lastSuccess.
// The real "dead conn self-heals in <=30s" behavior is only observable in E2E/production
// (via ResponseHeaderTimeout on the base transport).
type pollStampRoundTripper struct {
	base  http.RoundTripper
	watch *pollWatchdog
	now   func() time.Time
}

func (rt *pollStampRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if strings.Contains(req.URL.Path, "getUpdates") {
		ok := err == nil && resp != nil && resp.StatusCode/100 == 2
		es := ""
		if err != nil {
			es = err.Error()
		} else if resp != nil && resp.StatusCode/100 != 2 {
			es = fmt.Sprintf("status %d", resp.StatusCode)
		}
		rt.watch.onResult(rt.now(), ok, es)
	}
	return resp, err
}
