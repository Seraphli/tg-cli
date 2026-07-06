package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	startTime := time.Now().Add(-90 * time.Second)
	registerHealth(mux, startTime, "deadbeef")

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	for _, k := range []string{"version", "pid", "uptime_seconds", "binary_md5"} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing key %q in /health response: %v", k, body)
		}
	}
	if body["binary_md5"] != "deadbeef" {
		t.Errorf("binary_md5 = %v, want deadbeef", body["binary_md5"])
	}
	// uptime should reflect the elapsed time since startTime (~90s), proving it is computed live.
	if up, ok := body["uptime_seconds"].(float64); !ok || up < 89 {
		t.Errorf("uptime_seconds = %v, want >= 89", body["uptime_seconds"])
	}
}
