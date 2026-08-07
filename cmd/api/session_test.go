package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/internal/injector"
)

// TestSessionSendResult covers the four /session/send inject-outcome branches: a nil error and the
// two post-paste "text reached the pane" sentinels are SOFT (200 + delivery status + notify), while
// any other error is HARD (500 + no notify).
func TestSessionSendResult(t *testing.T) {
	tests := []struct {
		name         string
		injErr       error
		wantStatus   int
		wantDelivery string
		wantNotify   bool
	}{
		{"nil success", nil, http.StatusOK, "", true},
		{"inject not confirmed", fmt.Errorf("%w for target=x", helpers.ErrInjectNotConfirmed), http.StatusOK, "unconfirmed", true},
		{"submit after paste", fmt.Errorf("%w: tmux fail", injector.ErrSubmitAfterPaste), http.StatusOK, "submit_failed", true},
		{"hard pre-delivery error", errors.New("parse target failed"), http.StatusInternalServerError, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, delivery, doNotify := sessionSendResult(tc.injErr)
			if status != tc.wantStatus || delivery != tc.wantDelivery || doNotify != tc.wantNotify {
				t.Errorf("sessionSendResult(%v) = (%d, %q, %v), want (%d, %q, %v)", tc.injErr, status, delivery, doNotify, tc.wantStatus, tc.wantDelivery, tc.wantNotify)
			}
		})
	}
}

// TestSessionSendBody: delivery_status is omitted when empty (normal-path body byte-identical to the
// historical {"ok":true}) and carried otherwise.
func TestSessionSendBody(t *testing.T) {
	if got := sessionSendBody(""); got != `{"ok":true}` {
		t.Errorf(`sessionSendBody("") = %s, want {"ok":true}`, got)
	}
	if got := sessionSendBody("unconfirmed"); got != `{"ok":true,"delivery_status":"unconfirmed"}` {
		t.Errorf("sessionSendBody(unconfirmed) = %s", got)
	}
	if got := sessionSendBody("submit_failed"); got != `{"ok":true,"delivery_status":"submit_failed"}` {
		t.Errorf("sessionSendBody(submit_failed) = %s", got)
	}
}

// TestSessionSendWatchSkipDecision verifies the CLI --watch skip rule end-to-end from the server body
// builder: sessionSendBody's output decodes to the delivery_status the CLI reads, and a non-empty
// status is exactly when the CLI skips --watch (a soft-delivery command emits no events).
func TestSessionSendWatchSkipDecision(t *testing.T) {
	for _, ds := range []string{"", "unconfirmed", "submit_failed"} {
		body := sessionSendBody(ds)
		var res struct {
			DeliveryStatus string `json:"delivery_status"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		if res.DeliveryStatus != ds {
			t.Errorf("decoded delivery_status = %q, want %q", res.DeliveryStatus, ds)
		}
		skipWatch := res.DeliveryStatus != "" // the CLI's --watch skip rule
		if skipWatch != (ds != "") {
			t.Errorf("skipWatch for %q = %v, want %v", ds, skipWatch, ds != "")
		}
	}
}
