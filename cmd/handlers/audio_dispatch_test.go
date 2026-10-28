package handlers

import (
	"errors"
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
	bot, _ := newTrackingBot(t)
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
			assertAudioQueueEmpty(t)
			pinStatusClock(t)
			setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️"})
			bot, tc := newTrackingBot(t)
			bs := &types.BotState{Bot: bot}
			RegisterCallbackHandlers(bs)

			V := &tele.Message{ID: 1001, Chat: &tele.Chat{ID: 500, Type: tele.ChatPrivate}, Voice: &tele.Voice{File: tele.File{FileID: "orig-file-abc"}}}
			failureMsg := &tele.Message{ID: 4004, Chat: V.Chat, ReplyTo: V}

			var gotFileID, gotExt string
			downloadAudio = func(b *tele.Bot, fileID, ext string) (string, error) {
				gotFileID, gotExt = fileID, ext
				return writeTempFile(t, "audio-bytes"), nil
			}
			var gotEngine string
			var atEngine []apiCall
			engine := "sensevoice"
			transcribeWithEngineFn = func(path, engine2 string) (string, string, error) {
				gotEngine = engine2
				// R1: no t.Fatal/t.Error inside a stub that runs under handleAudioTranscription's own
				// call stack (Synchronous dispatch) — Fatalf's runtime.Goexit would unwind straight
				// past close(done)/wg.Wait()/audioQueueLeave(ticket), leaking the ticket and leaving
				// the ticker goroutine running against a server t.Cleanup has already closed. Just
				// capture the snapshot here; assert on it after ProcessUpdate returns (S6: proves the
				// Retrying edit happened strictly BEFORE the engine call).
				atEngine = tc.snapshot()
				return "transcribed", engine2, nil
			}
			var gotCtx tele.Context
			var puiCalled int
			processUserInputFn = func(bs *types.BotState, ctx tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
				puiCalled++
				gotCtx = ctx
				return nil
			}

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
				t.Errorf("synthetic context Message().ID = %d, want V.ID=%d (A9 struct-copy)", gotCtx.Message().ID, V.ID)
			}

			// S6 (TC9(a)): assert on the snapshot captured INSIDE the stub (atEngine), proving the
			// Retrying edit was already applied strictly BEFORE the engine call started — not just
			// before the LAST edit, which synchronous dispatch would prove anyway even if
			// reuseAudioStatus ran after the engine call.
			if len(atEngine) != 2 {
				t.Fatalf("calls recorded when the engine call started = %d, want exactly 2 (answerCallbackQuery + the Retrying edit already applied)", len(atEngine))
			}
			if atEngine[0].method != "answerCallbackQuery" || atEngine[1].method != "editMessageText" {
				t.Fatalf("calls recorded when the engine call started = %+v, want answerCallbackQuery then editMessageText", atEngine)
			}
			retryAtEngine := decodeCallParams(t, atEngine[1].body)
			if retryAtEngine["text"] != "⏳ Retrying with SenseVoice… 0s" || retryAtEngine["reply_markup"] != "{}" || atEngine[1].msgID != "4004" {
				t.Errorf("Retrying edit not yet correctly applied when the engine call started: %+v", retryAtEngine)
			}

			calls := tc.snapshot()
			if len(calls) != 3 {
				t.Fatalf("calls = %d, want exactly 3 (answerCallbackQuery + retry-status edit + final transcript edit), got %+v", len(calls), calls)
			}
			if calls[0].method != "answerCallbackQuery" {
				t.Fatalf("1st call method = %q, want answerCallbackQuery (A6: spinner cleared first)", calls[0].method)
			}
			if calls[1].method != "editMessageText" || calls[2].method != "editMessageText" {
				t.Fatalf("2nd/3rd call methods = %q, %q, want editMessageText, editMessageText (failureMsg is REUSED, never a new send)", calls[1].method, calls[2].method)
			}
			retryParams := decodeCallParams(t, calls[1].body)
			if retryParams["text"] != "⏳ Retrying with SenseVoice… 0s" {
				t.Errorf("1st edit (retry status) text = %q, want exactly the Retrying-at-0s text", retryParams["text"])
			}
			if retryParams["reply_markup"] != "{}" {
				t.Errorf("1st edit (retry status) reply_markup = %q, want exactly %q (buttons cleared in the SAME edit)", retryParams["reply_markup"], "{}")
			}
			finalParams := decodeCallParams(t, calls[2].body)
			if finalParams["text"] != "🗣️ transcribed" {
				t.Errorf("2nd edit (final) text = %q, want exactly %q", finalParams["text"], "🗣️ transcribed")
			}
			if finalParams["reply_markup"] != "{}" {
				t.Errorf("2nd edit (final) reply_markup = %q, want exactly %q (no buttons)", finalParams["reply_markup"], "{}")
			}
			if calls[1].msgID != "4004" || calls[2].msgID != "4004" {
				t.Errorf("both edits must target failureMsg (message_id=4004) only — no other message: retry msg_id=%q, final msg_id=%q", calls[1].msgID, calls[2].msgID)
			}
		})
	}
}

func TestVretryCallback_A9Chain_RetryAlsoFails_PreservesRecovery(t *testing.T) {
	assertAudioQueueEmpty(t)
	pinStatusClock(t)
	origDL, origTWE := downloadAudio, transcribeWithEngineFn
	t.Cleanup(func() { downloadAudio, transcribeWithEngineFn = origDL, origTWE })

	setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️", WhisperPath: "/bin/whisper", ModelPath: "/models/w"})
	bot, tc := newTrackingBot(t)
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

	calls := tc.snapshot()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want exactly 3 (answerCallbackQuery + retry-status edit + new failure edit), got %+v", len(calls), calls)
	}
	if calls[0].method != "answerCallbackQuery" {
		t.Fatalf("1st call method = %q, want answerCallbackQuery", calls[0].method)
	}
	// S9 (TC9b "again no other message was sent"): both edits after the spinner-clear target
	// failureMsg — previously only the LAST call was checked.
	if calls[1].method != "editMessageText" || calls[2].method != "editMessageText" {
		t.Fatalf("2nd/3rd call methods = %q, %q, want editMessageText, editMessageText", calls[1].method, calls[2].method)
	}
	if calls[1].msgID != "4004" || calls[2].msgID != "4004" {
		t.Errorf("both edits must target failureMsg (message_id=4004): retry msg_id=%q, final msg_id=%q", calls[1].msgID, calls[2].msgID)
	}
	final := decodeCallParams(t, calls[2].body)
	if final["text"] != "❌ boom retry-transcribe" {
		t.Errorf("new failure edit text = %q, want exactly %q", final["text"], "❌ boom retry-transcribe")
	}
	if !strings.Contains(final["reply_markup"], "vretry|whisper|"+strconv.Itoa(replyToID)+"|0") {
		t.Errorf("expected the NEW failure's retry button to still carry the captured N.ID=%d (SAME target recovery), got %q", replyToID, final["reply_markup"])
	}
}

func TestVretryCallback_APIForcedPath(t *testing.T) {
	assertAudioQueueEmpty(t)
	pinStatusClock(t)
	origDL, origTWE, origPUI := downloadAudio, transcribeWithEngineFn, processUserInputFn
	t.Cleanup(func() { downloadAudio, transcribeWithEngineFn, processUserInputFn = origDL, origTWE, origPUI })

	setupTestConfig(t, config.AppConfig{VoicePrefix: "🗣️"})
	bot, tc := newTrackingBot(t)
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
	calls := tc.snapshot()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want exactly 3 (answerCallbackQuery + retry-status edit + final transcript edit), got %+v", len(calls), calls)
	}
	if calls[0].method != "answerCallbackQuery" {
		t.Fatalf("1st call method = %q, want answerCallbackQuery", calls[0].method)
	}
	if calls[1].method != "editMessageText" || calls[2].method != "editMessageText" {
		t.Fatalf("2nd/3rd call methods = %q, %q, want editMessageText, editMessageText (failureMsg is REUSED, never a new send)", calls[1].method, calls[2].method)
	}
	if calls[1].msgID != "4004" || calls[2].msgID != "4004" {
		t.Errorf("both edits must target failureMsg (message_id=4004) only — no other message: retry msg_id=%q, final msg_id=%q", calls[1].msgID, calls[2].msgID)
	}
	retryParams := decodeCallParams(t, calls[1].body)
	if retryParams["text"] != "⏳ Retrying with API… 0s" {
		t.Errorf("1st edit (retry status) text = %q, want exactly the Retrying-with-API-at-0s text", retryParams["text"])
	}
	if retryParams["reply_markup"] != "{}" {
		t.Errorf("1st edit (retry status) reply_markup = %q, want exactly %q (buttons cleared in the same edit)", retryParams["reply_markup"], "{}")
	}
	finalParams := decodeCallParams(t, calls[2].body)
	if finalParams["text"] != "🗣️ api transcript" {
		t.Errorf("2nd edit (final) text = %q, want exactly %q", finalParams["text"], "🗣️ api transcript")
	}
	if finalParams["reply_markup"] != "{}" {
		t.Errorf("2nd edit (final) reply_markup = %q, want exactly %q (no buttons)", finalParams["reply_markup"], "{}")
	}
}
