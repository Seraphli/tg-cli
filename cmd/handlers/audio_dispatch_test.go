package handlers

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	tele "gopkg.in/telebot.v3"
)

// allowChat authorizes chatID (as a pairing.IsAllowed ID string) so the OnVoice/OnAudio/OnDocument
// handlers' pairing gate does not short-circuit before reaching the audio-routing logic under test.
func allowChat(t *testing.T, chatID int64) {
	t.Helper()
	creds := config.Credentials{
		PairingAllow: config.PairingAllow{IDs: []string{strconv.FormatInt(chatID, 10)}},
		Port:         12500,
		NameRouteMap: map[string]config.NameRoute{},
	}
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
}

// newRoutedBotState isolates config, wires an Offline capturing bot, and registers the real message
// handlers on it (RegisterMessageHandlers reads voicePrefix at registration time, so config must be set
// up first).
func newRoutedBotState(t *testing.T) (*tele.Bot, *types.BotState) {
	t.Helper()
	setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️"})
	bot, _ := newCapturingBot(t)
	// MergeBuffers is touched by processUserInput's very first line (the non-audio Document inject path
	// dispatches into it) even when no merge is active.
	bs := &types.BotState{Bot: bot, MergeBuffers: stores.NewMergeBufferStore(t.TempDir())}
	RegisterMessageHandlers(bs)
	return bot, bs
}

// TestAudioRouting_DispatchMatrix covers T6 Group D#10: dispatches real Updates through the registered
// OnVoice/OnAudio/OnDocument handlers (bot.ProcessUpdate) and asserts whether downloadAudio (the A3 seam)
// was reached, for each routing scenario the plan enumerates.
func TestAudioRouting_DispatchMatrix(t *testing.T) {
	origDL := downloadAudio
	t.Cleanup(func() { downloadAudio = origDL })

	type call struct{ fileID, ext string }
	mockDownload := func(t *testing.T) *[]call {
		var got []call
		downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) {
			got = append(got, call{fileID, ext})
			return "", errors.New("stop before real transcription")
		}
		return &got
	}

	t.Run("group voice -> downloadAudio called", func(t *testing.T) {
		bot, _ := newRoutedBotState(t)
		got := mockDownload(t)
		chat := &tele.Chat{ID: 100, Type: tele.ChatGroup}
		allowChat(t, chat.ID)
		msg := &tele.Message{ID: 10, Chat: chat, Sender: &tele.User{ID: 1}, Voice: &tele.Voice{File: tele.File{FileID: "v1"}}}
		bot.ProcessUpdate(tele.Update{Message: msg})
		if want := []call{{"v1", "ogg"}}; len(*got) != 1 || (*got)[0] != want[0] {
			t.Errorf("downloadAudio calls = %+v, want %+v", *got, want)
		}
	})

	t.Run("private reply voice -> downloadAudio called", func(t *testing.T) {
		bot, _ := newRoutedBotState(t)
		got := mockDownload(t)
		chat := &tele.Chat{ID: 101, Type: tele.ChatPrivate}
		allowChat(t, chat.ID)
		msg := &tele.Message{ID: 11, Chat: chat, Sender: &tele.User{ID: 1}, Voice: &tele.Voice{File: tele.File{FileID: "v2"}}, ReplyTo: &tele.Message{ID: 999, Chat: chat}}
		bot.ProcessUpdate(tele.Update{Message: msg})
		if want := []call{{"v2", "ogg"}}; len(*got) != 1 || (*got)[0] != want[0] {
			t.Errorf("downloadAudio calls = %+v, want %+v", *got, want)
		}
	})

	t.Run("group audio (msg.Audio) -> downloadAudio called", func(t *testing.T) {
		bot, _ := newRoutedBotState(t)
		got := mockDownload(t)
		chat := &tele.Chat{ID: 102, Type: tele.ChatGroup}
		allowChat(t, chat.ID)
		msg := &tele.Message{ID: 12, Chat: chat, Sender: &tele.User{ID: 1}, Audio: &tele.Audio{File: tele.File{FileID: "a1"}, FileName: "clip.mp3"}}
		bot.ProcessUpdate(tele.Update{Message: msg})
		if want := []call{{"a1", "mp3"}}; len(*got) != 1 || (*got)[0] != want[0] {
			t.Errorf("downloadAudio calls = %+v, want %+v", *got, want)
		}
	})

	t.Run("group audio-Document by MIME -> downloadAudio called", func(t *testing.T) {
		bot, _ := newRoutedBotState(t)
		got := mockDownload(t)
		chat := &tele.Chat{ID: 103, Type: tele.ChatGroup}
		allowChat(t, chat.ID)
		msg := &tele.Message{ID: 13, Chat: chat, Sender: &tele.User{ID: 1}, Document: &tele.Document{File: tele.File{FileID: "d1"}, MIME: "audio/mp3"}}
		bot.ProcessUpdate(tele.Update{Message: msg})
		if want := []call{{"d1", "mp3"}}; len(*got) != 1 || (*got)[0] != want[0] {
			t.Errorf("downloadAudio calls = %+v, want %+v", *got, want)
		}
	})

	t.Run("group audio-Document by extension -> downloadAudio called", func(t *testing.T) {
		bot, _ := newRoutedBotState(t)
		got := mockDownload(t)
		chat := &tele.Chat{ID: 104, Type: tele.ChatGroup}
		allowChat(t, chat.ID)
		msg := &tele.Message{ID: 14, Chat: chat, Sender: &tele.User{ID: 1}, Document: &tele.Document{File: tele.File{FileID: "d2"}, FileName: "clip.wav"}}
		bot.ProcessUpdate(tele.Update{Message: msg})
		if want := []call{{"d2", "wav"}}; len(*got) != 1 || (*got)[0] != want[0] {
			t.Errorf("downloadAudio calls = %+v, want %+v", *got, want)
		}
	})

	t.Run("private non-audio Document -> downloadAudio NOT called (inject path)", func(t *testing.T) {
		bot, _ := newRoutedBotState(t)
		got := mockDownload(t)
		chat := &tele.Chat{ID: 105, Type: tele.ChatPrivate}
		allowChat(t, chat.ID)
		msg := &tele.Message{ID: 15, Chat: chat, Sender: &tele.User{ID: 1}, Document: &tele.Document{File: tele.File{FileID: "d3"}, FileName: "report.pdf", MIME: "application/pdf"}}
		bot.ProcessUpdate(tele.Update{Message: msg})
		if len(*got) != 0 {
			t.Errorf("downloadAudio calls = %+v, want none (non-audio document must NOT transcribe)", *got)
		}
	})

	t.Run("private audio -> downloadAudio NOT called (dropped)", func(t *testing.T) {
		bot, _ := newRoutedBotState(t)
		got := mockDownload(t)
		chat := &tele.Chat{ID: 106, Type: tele.ChatPrivate}
		allowChat(t, chat.ID)
		msg := &tele.Message{ID: 16, Chat: chat, Sender: &tele.User{ID: 1}, Audio: &tele.Audio{File: tele.File{FileID: "a2"}}}
		bot.ProcessUpdate(tele.Update{Message: msg})
		if len(*got) != 0 {
			t.Errorf("downloadAudio calls = %+v, want none (private msg.Audio must be dropped)", *got)
		}
	})
}

// TestVretryCallback_RedeliversAndRoutesByCapturedTarget covers T6 Group D#11 (i)-(iii): a real "vretry"
// callback dispatch re-downloads V's (the original audio message's) FileID, forces the payload-encoded
// engine into transcribeWithEngineFn, and hands off to processUserInputFn with a synthetic context whose
// ReplyTo/ThreadID are the captured N.ID/threadID and whose Message().ID is V.ID (A9: recordPending/
// sendFeedback must use V.ID, never 0).
func TestVretryCallback_RedeliversAndRoutesByCapturedTarget(t *testing.T) {
	origDL, origTWE, origPUI := downloadAudio, transcribeWithEngineFn, processUserInputFn
	t.Cleanup(func() { downloadAudio, transcribeWithEngineFn, processUserInputFn = origDL, origTWE, origPUI })

	cases := []struct {
		name      string
		replyToID int
		threadID  int
	}{
		{"private voice-reply routes by N.ID", 2002, 0},
		{"group-notification-reply variant", 3003, 77},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️"})
			bot, bodies := newCapturingBot(t)
			bs := &types.BotState{Bot: bot}
			RegisterCallbackHandlers(bs)

			// V: the ORIGINAL audio message. Single-level ReplyTo==nil (a nested fixture would hide the
			// A1 bug per the plan's explicit instruction).
			V := &tele.Message{ID: 1001, Chat: &tele.Chat{ID: 500, Type: tele.ChatPrivate}, Voice: &tele.Voice{File: tele.File{FileID: "orig-file-abc"}}}
			failureMsg := &tele.Message{ID: 4004, Chat: V.Chat, ReplyTo: V}

			var gotFileID, gotExt string
			downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) {
				gotFileID, gotExt = fileID, ext
				return writeTempFile(t, "audio-bytes"), nil
			}
			var gotEngine string
			transcribeWithEngineFn = func(path, engine string) (string, string, error) {
				gotEngine = engine
				return "transcribed", engine, nil
			}
			var gotCtx tele.Context
			var puiCalled int
			processUserInputFn = func(bs *types.BotState, ctx tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
				puiCalled++
				gotCtx = ctx
				return nil
			}

			engine := "sensevoice"
			data := "\fvretry|" + engine + "|" + strconv.Itoa(c.replyToID) + "|" + strconv.Itoa(c.threadID)
			bot.ProcessUpdate(tele.Update{Callback: &tele.Callback{Sender: &tele.User{ID: 1}, Message: failureMsg, Data: data}})

			if gotFileID != V.Voice.FileID {
				t.Errorf("re-downloaded fileID = %q, want V's fileID %q", gotFileID, V.Voice.FileID)
			}
			if gotExt != "ogg" {
				t.Errorf("ext = %q, want ogg", gotExt)
			}
			if gotEngine != engine {
				t.Errorf("forced engine reaching transcribeWithEngineFn = %q, want %q", gotEngine, engine)
			}
			if puiCalled != 1 {
				t.Fatalf("processUserInputFn called %d times, want 1", puiCalled)
			}
			if gotCtx.Message().ReplyTo == nil || gotCtx.Message().ReplyTo.ID != c.replyToID {
				t.Errorf("routing reply target ID = %+v, want %d (captured N.ID)", gotCtx.Message().ReplyTo, c.replyToID)
			}
			if gotCtx.Message().ThreadID != c.threadID {
				t.Errorf("routing threadID = %d, want %d (captured)", gotCtx.Message().ThreadID, c.threadID)
			}
			if gotCtx.Message().ID != V.ID {
				t.Errorf("synthetic context Message().ID = %d, want V.ID=%d (A9 struct-copy: recordPending/sendFeedback must use V.ID, not 0)", gotCtx.Message().ID, V.ID)
			}
			// The vretry outcome edit must claim success only on an actual successful retry.
			if joined := strings.Join(*bodies, "\n"); !strings.Contains(joined, "Retried with SenseVoice") {
				t.Errorf("expected the success outcome edit (Retried with SenseVoice — see reply below), got %q", joined)
			}
		})
	}
}

// TestVretryCallback_A9Chain_RetryAlsoFails_PreservesRecovery covers T6 Group D#11's A9 chain + T12: when
// the forced-local retry ALSO fails, the NEW failure reply must (i) be a reply to V.ID, so a further
// vretry click can still recover V (Telegram re-attaches V as .ReplyTo of the new failure message), and
// (ii) carry the full configured-engine retry button set (here whisper only, the sole configured engine)
// still encoding the ORIGINAL captured N.ID (replyToID), not V.ID or 0 — so the SAME FileID+target chain
// stays recoverable indefinitely.
func TestVretryCallback_A9Chain_RetryAlsoFails_PreservesRecovery(t *testing.T) {
	origDL, origTWE := downloadAudio, transcribeWithEngineFn
	t.Cleanup(func() { downloadAudio, transcribeWithEngineFn = origDL, origTWE })

	// whisper is the ONLY configured engine, so the recovery button set on the new failure is {whisper}.
	setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️", WhisperPath: "/bin/whisper", ModelPath: "/models/w"})
	bot, bodies := newCapturingBot(t)
	bs := &types.BotState{Bot: bot}
	RegisterCallbackHandlers(bs)

	V := &tele.Message{ID: 1001, Chat: &tele.Chat{ID: 500, Type: tele.ChatPrivate}, Voice: &tele.Voice{File: tele.File{FileID: "orig-file-abc"}}}
	failureMsg := &tele.Message{ID: 4004, Chat: V.Chat, ReplyTo: V}

	downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
	transcribeWithEngineFn = func(path, engine string) (string, string, error) {
		return "", engine, errors.New("boom retry-transcribe")
	}

	replyToID := 2002
	data := "\fvretry|sensevoice|" + strconv.Itoa(replyToID) + "|0"
	bot.ProcessUpdate(tele.Update{Callback: &tele.Callback{Sender: &tele.User{ID: 1}, Message: failureMsg, Data: data}})

	joined := strings.Join(*bodies, "\n")
	if !strings.Contains(joined, "boom retry-transcribe") {
		t.Errorf("expected the retry-transcribe error in the new failure reply, got %q", joined)
	}
	if !strings.Contains(joined, fmt.Sprintf(`"reply_to_message_id":"%d"`, V.ID)) {
		t.Errorf("expected the NEW failure reply to be a reply to V.ID=%d, got %q", V.ID, joined)
	}
	if !strings.Contains(joined, "vretry|whisper|"+strconv.Itoa(replyToID)+"|0") {
		t.Errorf("expected the NEW failure's retry button to still carry the captured N.ID=%d (SAME target recovery), got %q", replyToID, joined)
	}
	// Failure-aware outcome edit: when the forced retry's own transcription failed, the button-message
	// edit must say the retry FAILED with that engine, never claim success.
	if !strings.Contains(joined, "Retry with SenseVoice failed") {
		t.Errorf("expected the failure-aware outcome edit (Retry with SenseVoice failed) when the forced retry itself failed, got %q", joined)
	}
	if strings.Contains(joined, "Retried with SenseVoice") {
		t.Errorf("outcome edit must NOT claim success (Retried with ...) when the forced retry failed, got %q", joined)
	}
}

// TestVretryCallback_APIForcedPath covers T12: a "vretry" callback carrying engine=api forces "api" into
// transcribeWithEngineFn (the shared handler dispatches api like any other engine) and the outcome edit
// uses the "API" label — the boss ruling makes api a first-class retry target, not just the local engines.
func TestVretryCallback_APIForcedPath(t *testing.T) {
	origDL, origTWE, origPUI := downloadAudio, transcribeWithEngineFn, processUserInputFn
	t.Cleanup(func() { downloadAudio, transcribeWithEngineFn, processUserInputFn = origDL, origTWE, origPUI })

	setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️"})
	bot, bodies := newCapturingBot(t)
	bs := &types.BotState{Bot: bot}
	RegisterCallbackHandlers(bs)

	V := &tele.Message{ID: 1001, Chat: &tele.Chat{ID: 500, Type: tele.ChatPrivate}, Audio: &tele.Audio{File: tele.File{FileID: "orig-file-api"}, FileName: "clip.mp3"}}
	failureMsg := &tele.Message{ID: 4004, Chat: V.Chat, ReplyTo: V}

	downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) { return writeTempFile(t, "audio-bytes"), nil }
	var gotEngine string
	transcribeWithEngineFn = func(path, engine string) (string, string, error) {
		gotEngine = engine
		return "api transcript", engine, nil
	}
	processUserInputFn = func(bs *types.BotState, ctx tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
		return nil
	}

	data := "\fvretry|api|2002|0"
	bot.ProcessUpdate(tele.Update{Callback: &tele.Callback{Sender: &tele.User{ID: 1}, Message: failureMsg, Data: data}})

	if gotEngine != "api" {
		t.Errorf("forced engine reaching transcribeWithEngineFn = %q, want api", gotEngine)
	}
	if joined := strings.Join(*bodies, "\n"); !strings.Contains(joined, "Retried with API") {
		t.Errorf("expected the API-labeled success outcome edit (Retried with API — see reply below), got %q", joined)
	}
}
