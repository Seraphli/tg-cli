package voice

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
)

// isolateHome points HOME (and USERPROFILE, for portability) at a fresh temp dir. retainVoiceFile
// unconditionally reads/writes ~/.tg-cli/voice-cache and IGNORES any config-dir override (IRON RULE
// A5), so every test reaching TranscribeWithEngine/Transcribe must isolate HOME to avoid touching the
// real user's cache. It also isolates config.LoadAppConfig's config dir, since GetConfigDir() falls
// back to os.UserHomeDir() when config.ConfigDir is unset.
func isolateHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func writeTempAudio(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.ogg")
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	return path
}

// TestTranscribeAPI_Success covers T6 Group A#1: the "api" engine POSTs a multipart form with the
// file and model, carries the Bearer key, and returns the parsed "text" field.
func TestTranscribeAPI_Success(t *testing.T) {
	isolateHome(t)
	var gotMethod, gotPath, gotAuth, gotContentType, gotModel string
	var gotFileBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		gotModel = r.FormValue("model")
		if f, _, err := r.FormFile("file"); err != nil {
			t.Errorf("FormFile(file): %v", err)
		} else {
			gotFileBytes, _ = io.ReadAll(f)
			f.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"text":"hello world"}`)
	}))
	defer srv.Close()

	cfg := config.AppConfig{VoiceAPIBaseURL: srv.URL, VoiceAPIModel: "test-model", VoiceAPIKey: "test-key"}
	if err := config.SaveAppConfig(cfg); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}

	text, err := transcribeAPI(writeTempAudio(t, "fake ogg bytes"))
	if err != nil {
		t.Fatalf("transcribeAPI: %v", err)
	}
	if text != "hello world" {
		t.Errorf("text = %q, want %q", text, "hello world")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/audio/transcriptions" {
		t.Errorf("path = %q, want /v1/audio/transcriptions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-key")
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data prefix", gotContentType)
	}
	if gotModel != "test-model" {
		t.Errorf("model field = %q, want test-model", gotModel)
	}
	if string(gotFileBytes) != "fake ogg bytes" {
		t.Errorf("uploaded file bytes = %q, want %q", gotFileBytes, "fake ogg bytes")
	}
}

// TestTranscribeAPI_HTTPError covers T6 Group A#2: a non-2xx response surfaces an error that includes
// the HTTP status code.
func TestTranscribeAPI_HTTPError(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "internal error")
	}))
	defer srv.Close()

	cfg := config.AppConfig{VoiceAPIBaseURL: srv.URL, VoiceAPIModel: "test-model", VoiceAPIKey: "test-key"}
	if err := config.SaveAppConfig(cfg); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}

	_, err := transcribeAPI(writeTempAudio(t, "fake ogg bytes"))
	if err == nil {
		t.Fatal("expected an error for an HTTP 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want it to mention status 500", err)
	}
}

// TestTranscribeAPI_Timeout covers T6 Group A#3: a client with a short timeout against a slow server
// surfaces a timeout error.
func TestTranscribeAPI_Timeout(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"text":"too late"}`)
	}))
	defer srv.Close()

	origClient := apiHTTPClient
	apiHTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	t.Cleanup(func() { apiHTTPClient = origClient })

	cfg := config.AppConfig{VoiceAPIBaseURL: srv.URL, VoiceAPIModel: "test-model", VoiceAPIKey: "test-key"}
	if err := config.SaveAppConfig(cfg); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}

	_, err := transcribeAPI(writeTempAudio(t, "fake ogg bytes"))
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "Client.Timeout") && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("error = %v, want a client-timeout-shaped error", err)
	}
}

// TestTranscribeWithEngine_APIRouting covers T6 Group A#4: TranscribeWithEngine(path, "api") routes to
// the API path and returns engine=="api".
func TestTranscribeWithEngine_APIRouting(t *testing.T) {
	isolateHome(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"text":"from api engine"}`)
	}))
	defer srv.Close()

	cfg := config.AppConfig{VoiceAPIBaseURL: srv.URL, VoiceAPIModel: "test-model", VoiceAPIKey: "test-key", VoiceEngine: "whisper"}
	if err := config.SaveAppConfig(cfg); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}

	text, engine, err := TranscribeWithEngine(writeTempAudio(t, "fake ogg bytes"), "api")
	if err != nil {
		t.Fatalf("TranscribeWithEngine: %v", err)
	}
	if engine != "api" {
		t.Errorf("engine = %q, want api", engine)
	}
	if text != "from api engine" {
		t.Errorf("text = %q, want %q", text, "from api engine")
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1", calls)
	}
}

// TestTranscribe_DelegatesToConfiguredEngine covers T6 Group A#4: Transcribe (no explicit engine arg)
// must delegate to cfg.VoiceEngine, so setting it to "api" must reach the API path.
func TestTranscribe_DelegatesToConfiguredEngine(t *testing.T) {
	isolateHome(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"text":"delegated"}`)
	}))
	defer srv.Close()

	cfg := config.AppConfig{VoiceAPIBaseURL: srv.URL, VoiceAPIModel: "test-model", VoiceAPIKey: "test-key", VoiceEngine: "api"}
	if err := config.SaveAppConfig(cfg); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}

	text, engine, err := Transcribe(writeTempAudio(t, "fake ogg bytes"))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if engine != "api" {
		t.Errorf("engine = %q, want api (Transcribe must delegate to cfg.VoiceEngine)", engine)
	}
	if text != "delegated" {
		t.Errorf("text = %q, want %q", text, "delegated")
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (Transcribe must reach the API path)", calls)
	}
}

// TestTranscribeWithEngine_LocalEngineLabels covers T6 Group A#4's local-engine arms without touching
// real whisper/sherpa-onnx binaries: with no local engine configured, TranscribeWithEngine still returns
// the requested engine's label (the dispatch contract), just with a config error instead of a binary
// invocation.
func TestTranscribeWithEngine_LocalEngineLabels(t *testing.T) {
	isolateHome(t)
	if err := config.SaveAppConfig(config.AppConfig{}); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}
	audioPath := writeTempAudio(t, "fake ogg bytes")

	cases := []struct{ requested, wantEngine string }{
		{"sensevoice", "sensevoice"},
		{"whisper", "whisper"},
		{"unknown-engine", "whisper"}, // default case falls back to whisper's label
	}
	for _, c := range cases {
		_, engine, err := TranscribeWithEngine(audioPath, c.requested)
		if engine != c.wantEngine {
			t.Errorf("TranscribeWithEngine(%q) engine = %q, want %q", c.requested, engine, c.wantEngine)
		}
		if err == nil {
			t.Errorf("TranscribeWithEngine(%q) expected a config error (no local engine configured), got nil", c.requested)
		}
	}
}
