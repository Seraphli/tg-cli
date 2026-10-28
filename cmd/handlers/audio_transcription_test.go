package handlers

import (
	"errors"
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

func TestHandleAudioTranscription_FailureCarriesRetryMarkup(t *testing.T) {
	origDL, origTF := downloadAudio, transcribeFn
	t.Cleanup(func() { downloadAudio, transcribeFn = origDL, origTF })

	localCfg := config.AppConfig{VoiceEngine: "whisper", SherpaOnnxPath: "/bin/sherpa", SenseVoiceModelPath: "/models/sv"}
	noLocalCfg := config.AppConfig{VoiceEngine: "whisper"}

	t.Run("download error with local engine -> vretry button", func(t *testing.T) {
		assertAudioQueueEmpty(t)
		setupTestConfig(t, localCfg)
		bot, tc := newTrackingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return "", errors.New("boom download") }
		if err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil); err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		assertAudioQueueEmpty(t) // S7: queue released on a download error.
		calls := tc.snapshot()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2 (initial send + failure edit), got %+v", len(calls), calls)
		}
		if calls[0].method != "sendMessage" || calls[1].method != "editMessageText" {
			t.Fatalf("methods = %q, %q, want sendMessage then editMessageText", calls[0].method, calls[1].method)
		}
		final := decodeCallParams(t, calls[1].body)
		if final["message_id"] != calls[0].msgID {
			t.Errorf("failure edit message_id = %q, want the SAME message_id the initial send got (%q) — proves the SAME message was edited, not a new one sent", final["message_id"], calls[0].msgID)
		}
		if final["text"] != "❌ boom download" {
			t.Errorf("final edit text = %q, want exactly %q", final["text"], "❌ boom download")
		}
		if !strings.Contains(final["reply_markup"], "vretry|sensevoice|777|42") {
			t.Errorf("final edit reply_markup = %q, want a vretry button (sensevoice, replyTo=777, thread=42)", final["reply_markup"])
		}
	})

	t.Run("transcribe error with local engine -> vretry button", func(t *testing.T) {
		assertAudioQueueEmpty(t)
		pinStatusClock(t)
		setupTestConfig(t, localCfg)
		bot, tc := newTrackingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
		transcribeFn = func(path string) (string, string, error) { return "", "whisper", errors.New("boom transcribe") }
		if err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil); err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		assertAudioQueueEmpty(t) // S7: queue released on a transcribe error.
		calls := tc.snapshot()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2, got %+v", len(calls), calls)
		}
		if calls[0].method != "sendMessage" {
			t.Errorf("1st call method = %q, want sendMessage", calls[0].method)
		}
		if calls[1].method != "editMessageText" {
			t.Errorf("2nd call method = %q, want editMessageText", calls[1].method)
		}
		final := decodeCallParams(t, calls[1].body)
		if final["message_id"] != calls[0].msgID {
			t.Errorf("failure edit message_id = %q, want the SAME message_id the initial send got (%q) — proves the SAME message was edited, not a new one sent", final["message_id"], calls[0].msgID)
		}
		if final["text"] != "❌ boom transcribe" {
			t.Errorf("final edit text = %q, want exactly %q", final["text"], "❌ boom transcribe")
		}
		if !strings.Contains(final["reply_markup"], "vretry|sensevoice|777|42") {
			t.Errorf("final edit reply_markup = %q, want a vretry button", final["reply_markup"])
		}
	})

	t.Run("empty text with local engine -> vretry button", func(t *testing.T) {
		assertAudioQueueEmpty(t)
		pinStatusClock(t)
		setupTestConfig(t, localCfg)
		bot, tc := newTrackingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
		transcribeFn = func(path string) (string, string, error) { return "", "whisper", nil }
		if err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil); err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		assertAudioQueueEmpty(t) // S7: queue released when transcription produced empty text.
		calls := tc.snapshot()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2, got %+v", len(calls), calls)
		}
		if calls[0].method != "sendMessage" {
			t.Errorf("1st call method = %q, want sendMessage", calls[0].method)
		}
		if calls[1].method != "editMessageText" {
			t.Errorf("2nd call method = %q, want editMessageText", calls[1].method)
		}
		final := decodeCallParams(t, calls[1].body)
		if final["message_id"] != calls[0].msgID {
			t.Errorf("failure edit message_id = %q, want the SAME message_id the initial send got (%q) — proves the SAME message was edited, not a new one sent", final["message_id"], calls[0].msgID)
		}
		if final["text"] != "❌ transcription produced empty text" {
			t.Errorf("final edit text = %q, want exactly %q", final["text"], "❌ transcription produced empty text")
		}
		if !strings.Contains(final["reply_markup"], "vretry|sensevoice|777|42") {
			t.Errorf("final edit reply_markup = %q, want a vretry button", final["reply_markup"])
		}
	})

	t.Run("all engines configured -> one button per engine including the failed one", func(t *testing.T) {
		assertAudioQueueEmpty(t)
		pinStatusClock(t)
		setupTestConfig(t, config.AppConfig{VoiceEngine: "api", VoiceAPIBaseURL: "https://stt.example", WhisperPath: "/bin/whisper", ModelPath: "/models/w", SherpaOnnxPath: "/bin/sherpa", SenseVoiceModelPath: "/models/sv"})
		bot, tc := newTrackingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
		transcribeFn = func(path string) (string, string, error) { return "", "api", errors.New("boom api") }
		if err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil); err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		calls := tc.snapshot()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2, got %+v", len(calls), calls)
		}
		if calls[0].method != "sendMessage" {
			t.Errorf("1st call method = %q, want sendMessage", calls[0].method)
		}
		if calls[1].method != "editMessageText" {
			t.Errorf("2nd call method = %q, want editMessageText", calls[1].method)
		}
		final := decodeCallParams(t, calls[1].body)
		if final["message_id"] != calls[0].msgID {
			t.Errorf("failure edit message_id = %q, want the SAME message_id the initial send got (%q) — proves the SAME message was edited, not a new one sent", final["message_id"], calls[0].msgID)
		}
		for _, want := range []string{"vretry|api|777|42", "vretry|whisper|777|42", "vretry|sensevoice|777|42"} {
			if !strings.Contains(final["reply_markup"], want) {
				t.Errorf("final edit reply_markup = %q, want retry button %q (one per configured engine, including the failed api)", final["reply_markup"], want)
			}
		}
	})

	t.Run("no engine configured -> no button, explicit message", func(t *testing.T) {
		assertAudioQueueEmpty(t)
		setupTestConfig(t, noLocalCfg)
		bot, tc := newTrackingBot(t)
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return "", errors.New("boom download") }
		if err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil); err != nil {
			t.Fatalf("handleAudioTranscription: %v", err)
		}
		calls := tc.snapshot()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2, got %+v", len(calls), calls)
		}
		if calls[0].method != "sendMessage" {
			t.Errorf("1st call method = %q, want sendMessage", calls[0].method)
		}
		if calls[1].method != "editMessageText" {
			t.Errorf("2nd call method = %q, want editMessageText", calls[1].method)
		}
		final := decodeCallParams(t, calls[1].body)
		if final["message_id"] != calls[0].msgID {
			t.Errorf("failure edit message_id = %q, want the SAME message_id the initial send got (%q) — proves the SAME message was edited, not a new one sent", final["message_id"], calls[0].msgID)
		}
		if final["text"] != "❌ boom download (no transcription engine configured to retry with)" {
			t.Errorf("final edit text = %q, want exactly %q", final["text"], "❌ boom download (no transcription engine configured to retry with)")
		}
		if final["reply_markup"] != "{}" {
			t.Errorf("final edit reply_markup = %q, want exactly %q (no buttons, markup still explicit per SPEC section 2 — R3: Contains/!Contains could pass even if markup were omitted entirely, which SPEC forbids)", final["reply_markup"], "{}")
		}
	})
}

func TestHandleAudioTranscription_SuccessHandsOff(t *testing.T) {
	assertAudioQueueEmpty(t)
	pinStatusClock(t)
	setupTestConfig(t, config.AppConfig{VoiceEngine: "whisper"})
	bot, tc := newTrackingBot(t)

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

	if err := handleAudioTranscription(&types.BotState{}, newAudioCtx(bot), bot, "file1", "ogg", "🗣️", "", nil); err != nil {
		t.Fatalf("handleAudioTranscription: %v", err)
	}
	assertAudioQueueEmpty(t)
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

	// TC7: exactly ONE sendMessage (the initial status) and ONE editMessageText (the final transcript),
	// both against the SAME message, across the ENTIRE run.
	calls := tc.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want exactly 2 (one send, one edit), got %+v", len(calls), calls)
	}
	if calls[0].method != "sendMessage" {
		t.Errorf("1st call method = %q, want sendMessage", calls[0].method)
	}
	sendParams := decodeCallParams(t, calls[0].body)
	if sendParams["text"] != "⏳ Transcribing with Whisper… 0s" {
		t.Errorf("1st call (initial status) text = %q, want exactly the running-at-0s text", sendParams["text"])
	}
	if calls[1].method != "editMessageText" {
		t.Errorf("2nd call method = %q, want editMessageText", calls[1].method)
	}
	editParams := decodeCallParams(t, calls[1].body)
	if editParams["message_id"] != calls[0].msgID {
		t.Errorf("2nd call message_id = %q, want the SAME message_id the initial send got (%q) — proves the SAME message was edited, not a new one sent", editParams["message_id"], calls[0].msgID)
	}
	if editParams["text"] != "🗣️ hello world" {
		t.Errorf("2nd call (final edit) text = %q, want exactly %q", editParams["text"], "🗣️ hello world")
	}
	if editParams["reply_markup"] != "{}" {
		t.Errorf("2nd call (final edit) reply_markup = %q, want exactly %q (no buttons)", editParams["reply_markup"], "{}")
	}

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
