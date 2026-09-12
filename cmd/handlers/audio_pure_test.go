package handlers

import (
	"testing"

	"github.com/Seraphli/tg-cli/internal/config"
	tele "gopkg.in/telebot.v3"
)

// TestConfiguredRetryEngines covers T12: the failure reply offers one retry button per fully-configured
// engine, in a stable (api, whisper, sensevoice) order, INCLUDING the engine that just failed (every
// configured engine is retryable per the boss ruling). An unconfigured engine gets no button.
func TestConfiguredRetryEngines(t *testing.T) {
	allThree := config.AppConfig{VoiceAPIBaseURL: "https://stt.example", SherpaOnnxPath: "/bin/sherpa", SenseVoiceModelPath: "/models/sv", WhisperPath: "/bin/whisper", ModelPath: "/models/w"}
	onlyAPI := config.AppConfig{VoiceAPIBaseURL: "https://stt.example"}
	onlyWhisper := config.AppConfig{WhisperPath: "/bin/whisper", ModelPath: "/models/w"}
	onlySensevoice := config.AppConfig{SherpaOnnxPath: "/bin/sherpa", SenseVoiceModelPath: "/models/sv"}
	apiPlusSensevoice := config.AppConfig{VoiceAPIBaseURL: "https://stt.example", SherpaOnnxPath: "/bin/sherpa", SenseVoiceModelPath: "/models/sv"}
	none := config.AppConfig{}

	cases := []struct {
		name string
		cfg  config.AppConfig
		want []string
	}{
		{"all three configured -> api, whisper, sensevoice", allThree, []string{"api", "whisper", "sensevoice"}},
		{"only api configured -> api", onlyAPI, []string{"api"}},
		{"only whisper configured -> whisper", onlyWhisper, []string{"whisper"}},
		{"only sensevoice configured -> sensevoice", onlySensevoice, []string{"sensevoice"}},
		{"api + sensevoice preserves stable order", apiPlusSensevoice, []string{"api", "sensevoice"}},
		{"nothing configured -> none", none, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := configuredRetryEngines(c.cfg)
			if len(got) != len(c.want) {
				t.Fatalf("configuredRetryEngines(%+v) = %v, want %v", c.cfg, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("configuredRetryEngines(%+v) = %v, want %v", c.cfg, got, c.want)
				}
			}
		})
	}
}

// TestBuildRetryButtons covers T12: one "🔁 Retry with <engine>" button per configured engine (each on
// its own row) with the engine-specific label and a framed callback_data ("\fvretry|" + payload,
// telebot's 8-byte "\fvretry|" prefix) within Telegram's 64-byte limit for 10-digit IDs; an empty engine
// list yields a nil markup (caller falls back to a plain error reply).
func TestBuildRetryButtons(t *testing.T) {
	t.Run("nil markup when no engines", func(t *testing.T) {
		if got := buildRetryButtons(nil, 1, 2); got != nil {
			t.Errorf("buildRetryButtons(nil) = %+v, want nil", got)
		}
	})

	t.Run("one row per engine with engine-specific labels and payloads", func(t *testing.T) {
		engines := []string{"api", "whisper", "sensevoice"}
		wantLabel := map[string]string{"api": "🔁 Retry with API", "whisper": "🔁 Retry with Whisper", "sensevoice": "🔁 Retry with SenseVoice"}
		menu := buildRetryButtons(engines, 999999999, 999999999)
		if len(menu.InlineKeyboard) != len(engines) {
			t.Fatalf("expected %d rows (one button per engine), got layout %+v", len(engines), menu.InlineKeyboard)
		}
		for i, engine := range engines {
			if len(menu.InlineKeyboard[i]) != 1 {
				t.Fatalf("row %d: expected exactly one button, got %+v", i, menu.InlineKeyboard[i])
			}
			btn := menu.InlineKeyboard[i][0]
			if btn.Text != wantLabel[engine] {
				t.Errorf("row %d label = %q, want %q", i, btn.Text, wantLabel[engine])
			}
			wantData := engine + "|999999999|999999999"
			if btn.Data != wantData {
				t.Errorf("row %d payload = %q, want %q", i, btn.Data, wantData)
			}
			if framed := "\f" + btn.Unique + "|" + btn.Data; len(framed) > 64 {
				t.Errorf("row %d framed callback_data length = %d, want <= 64 (Telegram limit); framed=%q", i, len(framed), framed)
			}
		}
	})
}

// TestIsAudioDocument covers T6 Group B#7: MIME-prefix and extension-based audio detection.
func TestIsAudioDocument(t *testing.T) {
	cases := []struct {
		name string
		doc  *tele.Document
		want bool
	}{
		{"nil doc", nil, false},
		{"audio mime prefix", &tele.Document{MIME: "audio/ogg"}, true},
		{"mp3 ext", &tele.Document{FileName: "clip.mp3"}, true},
		{"m4a ext", &tele.Document{FileName: "clip.m4a"}, true},
		{"ogg ext", &tele.Document{FileName: "clip.ogg"}, true},
		{"oga ext", &tele.Document{FileName: "clip.oga"}, true},
		{"wav ext", &tele.Document{FileName: "clip.wav"}, true},
		{"flac ext", &tele.Document{FileName: "clip.flac"}, true},
		{"opus ext", &tele.Document{FileName: "clip.opus"}, true},
		{"aac ext", &tele.Document{FileName: "clip.aac"}, true},
		{"wma ext", &tele.Document{FileName: "clip.wma"}, true},
		{"webm ext", &tele.Document{FileName: "clip.webm"}, true},
		{"amr ext", &tele.Document{FileName: "clip.amr"}, true},
		{"3gp ext", &tele.Document{FileName: "clip.3gp"}, true},
		{"uppercase ext", &tele.Document{FileName: "CLIP.MP3"}, true},
		{"pdf mime+ext", &tele.Document{MIME: "application/pdf", FileName: "doc.pdf"}, false},
		{"no mime no ext", &tele.Document{FileName: "clip"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAudioDocument(c.doc); got != c.want {
				t.Errorf("isAudioDocument(%+v) = %v, want %v", c.doc, got, c.want)
			}
		})
	}
}
