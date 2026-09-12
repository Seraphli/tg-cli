package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// setupTestConfig isolates HOME (retainVoiceFile ignores any config-dir override — IRON RULE A5 — and
// GetConfigDir() falls back to os.UserHomeDir() when config.ConfigDir is unset, so one HOME isolation
// covers both) and persists cfg via SaveAppConfig, which also updates config's in-process cache so
// production code's config.LoadAppConfig() sees it regardless of file-mtime races.
func setupTestConfig(t *testing.T, cfg config.AppConfig) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := config.SaveAppConfig(cfg); err != nil {
		t.Fatalf("SaveAppConfig: %v", err)
	}
}

// newCapturingBot returns an Offline telebot bot wired to a fake Telegram Bot API server that records
// every raw request body it receives (send/edit/respond calls all land here).
func newCapturingBot(t *testing.T) (*tele.Bot, *[]string) {
	t.Helper()
	bodies := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":9001,"chat":{"id":1}}}`)
	}))
	t.Cleanup(srv.Close)
	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: srv.URL, Offline: true, Synchronous: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot, bodies
}

// writeTempFile writes content to a throwaway temp file and returns its path.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "audio-*.ogg")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()
	return f.Name()
}

// newAudioCtx builds a synthetic tele.Context for a private-chat audio message replying to msg 777 on
// thread 42 — the reply target/thread that sendTranscriptionFailure's vretry button must capture.
func newAudioCtx(bot *tele.Bot) tele.Context {
	msg := &tele.Message{
		ID:       555,
		Chat:     &tele.Chat{ID: 1, Type: tele.ChatPrivate},
		ReplyTo:  &tele.Message{ID: 777, Chat: &tele.Chat{ID: 1}},
		ThreadID: 42,
	}
	return bot.NewContext(tele.Update{Message: msg})
}

// TestHandleAudioTranscription_FailureCarriesRetryMarkup covers T6 Group C#8 + T12: every failure mode
// (download error, transcribe error, empty transcription) returns transcribed=false, replies with the
// error and one vretry inline button per fully-configured engine, each carrying the captured reply/thread
// IDs. With NO engine configured, the reply carries no button and states so.
func TestHandleAudioTranscription_FailureCarriesRetryMarkup(t *testing.T) {
	origDL, origTF := downloadAudio, transcribeFn
	t.Cleanup(func() { downloadAudio, transcribeFn = origDL, origTF })

	// Only sensevoice is fully configured here, so the button set is exactly {sensevoice}.
	localCfg := config.AppConfig{VoiceEngine: "whisper", SherpaOnnxPath: "/bin/sherpa", SenseVoiceModelPath: "/models/sv"}
	noLocalCfg := config.AppConfig{VoiceEngine: "whisper"}

	t.Run("download error with local engine -> vretry button", func(t *testing.T) {
		setupTestConfig(t, localCfg)
		bot, bodies := newCapturingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return "", errors.New("boom download") }
		transcribed, err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "")
		if err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		if transcribed {
			t.Error("transcribed = true, want false on download failure")
		}
		joined := strings.Join(*bodies, "\n")
		if !strings.Contains(joined, "boom download") {
			t.Errorf("expected the download error in the reply, got %q", joined)
		}
		if !strings.Contains(joined, "vretry|sensevoice|777|42") {
			t.Errorf("expected a vretry button (sensevoice, replyTo=777, thread=42), got %q", joined)
		}
	})

	t.Run("transcribe error with local engine -> vretry button", func(t *testing.T) {
		setupTestConfig(t, localCfg)
		bot, bodies := newCapturingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
		transcribeFn = func(path string) (string, string, error) { return "", "whisper", errors.New("boom transcribe") }
		transcribed, err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "")
		if err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		if transcribed {
			t.Error("transcribed = true, want false on transcribe failure")
		}
		joined := strings.Join(*bodies, "\n")
		if !strings.Contains(joined, "boom transcribe") {
			t.Errorf("expected the transcribe error in the reply, got %q", joined)
		}
		if !strings.Contains(joined, "vretry|sensevoice|777|42") {
			t.Errorf("expected a vretry button, got %q", joined)
		}
	})

	t.Run("empty text with local engine -> vretry button", func(t *testing.T) {
		setupTestConfig(t, localCfg)
		bot, bodies := newCapturingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
		transcribeFn = func(path string) (string, string, error) { return "", "whisper", nil }
		transcribed, err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "")
		if err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		if transcribed {
			t.Error("transcribed = true, want false on empty text")
		}
		joined := strings.Join(*bodies, "\n")
		if !strings.Contains(joined, "empty text") {
			t.Errorf("expected the empty-text error in the reply, got %q", joined)
		}
		if !strings.Contains(joined, "vretry|sensevoice|777|42") {
			t.Errorf("expected a vretry button, got %q", joined)
		}
	})

	t.Run("all engines configured -> one button per engine including the failed one", func(t *testing.T) {
		setupTestConfig(t, config.AppConfig{VoiceEngine: "api", VoiceAPIBaseURL: "https://stt.example", WhisperPath: "/bin/whisper", ModelPath: "/models/w", SherpaOnnxPath: "/bin/sherpa", SenseVoiceModelPath: "/models/sv"})
		bot, bodies := newCapturingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
		transcribeFn = func(path string) (string, string, error) { return "", "api", errors.New("boom api") }
		transcribed, err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "")
		if err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		if transcribed {
			t.Error("transcribed = true, want false on transcribe failure")
		}
		joined := strings.Join(*bodies, "\n")
		// The failed engine (api) MUST still get a button, alongside the other configured engines.
		for _, want := range []string{"vretry|api|777|42", "vretry|whisper|777|42", "vretry|sensevoice|777|42"} {
			if !strings.Contains(joined, want) {
				t.Errorf("expected retry button %q (one per configured engine, including the failed api), got %q", want, joined)
			}
		}
	})

	t.Run("no engine configured -> no button, explicit message", func(t *testing.T) {
		setupTestConfig(t, noLocalCfg)
		bot, bodies := newCapturingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return "", errors.New("boom download") }
		transcribed, err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "")
		if err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		if transcribed {
			t.Error("transcribed = true, want false on failure")
		}
		joined := strings.Join(*bodies, "\n")
		if !strings.Contains(joined, "no transcription engine configured to retry with") {
			t.Errorf("expected the no-engine message, got %q", joined)
		}
		if strings.Contains(joined, "vretry") {
			t.Errorf("expected NO vretry button when no engine is configured, got %q", joined)
		}
	})
}

// TestHandleAudioTranscription_SuccessHandsOff covers T6 Group C#9 + T11: a successful transcription
// returns transcribed=true, emits the success log (engine + duration + transcribed text), hands off to
// processUserInputFn (the permitted A3-style seam) with the transcribed text, isVoice=true and the
// configured voicePrefix, and sends NO vretry-failure reply.
func TestHandleAudioTranscription_SuccessHandsOff(t *testing.T) {
	setupTestConfig(t, config.AppConfig{VoiceEngine: "whisper"})
	bot, bodies := newCapturingBot(t)

	// Capture the success log line: debug=true so Info entries land in the file (writeLog is debug-gated).
	logPath := filepath.Join(t.TempDir(), "handler.log")
	logger.Init(logPath, true)
	t.Cleanup(func() { logger.Init("", false) })

	origDL, origTF, origPUI := downloadAudio, transcribeFn, processUserInputFn
	t.Cleanup(func() { downloadAudio, transcribeFn, processUserInputFn = origDL, origTF, origPUI })

	downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
	transcribeFn = func(path string) (string, string, error) { return "hello world", "whisper", nil }

	var gotText, gotPrefix string
	var gotIsVoice bool
	var called int
	processUserInputFn = func(bs *types.BotState, c tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
		called++
		gotText, gotIsVoice, gotPrefix = text, isVoice, voicePrefix
		return nil
	}

	transcribed, err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "")
	if err != nil {
		t.Fatalf("handleAudioTranscription: %v", err)
	}
	if !transcribed {
		t.Error("transcribed = false, want true on success")
	}
	if called != 1 {
		t.Fatalf("processUserInputFn called %d times, want 1", called)
	}
	if gotText != "hello world" {
		t.Errorf("text = %q, want %q", gotText, "hello world")
	}
	if !gotIsVoice {
		t.Error("isVoice = false, want true")
	}
	if gotPrefix != "🗣️" {
		t.Errorf("voicePrefix = %q, want 🗣️", gotPrefix)
	}
	if len(*bodies) != 0 {
		t.Errorf("expected NO bot API calls on the success path (no vretry reply), got %d: %v", len(*bodies), *bodies)
	}
	// T11: the restored success log carries the engine, the duration, and the transcribed text.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "Voice transcribed: engine=whisper") {
		t.Errorf("missing the success log with engine, log=%q", log)
	}
	if !strings.Contains(log, "duration=") {
		t.Errorf("missing the duration field in the success log, log=%q", log)
	}
	if !strings.Contains(log, "text=hello world") {
		t.Errorf("missing the transcribed text in the success log, log=%q", log)
	}
}
