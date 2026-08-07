package helpers

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/markdown"
	tele "gopkg.in/telebot.v3"
)

// smokeCredentials holds the minimal credential fields needed for the smoke test.
type smokeCredentials struct {
	BotToken     string `json:"botToken"`
	PairingAllow struct {
		DefaultChatID string `json:"defaultChatId"`
	} `json:"pairingAllow"`
}

// buildWideTableMD builds a 20-column markdown pipe table (Telegram's documented max).
// Header row: C1..C20, two data rows with sequential values.
func buildWideTableMD() string {
	// Header row
	headers := make([]string, 20)
	for i := range headers {
		headers[i] = fmt.Sprintf("C%d", i+1)
	}
	// Separator row
	seps := make([]string, 20)
	for i := range seps {
		seps[i] = "---"
	}
	// Data rows
	row1 := make([]string, 20)
	row2 := make([]string, 20)
	for i := range row1 {
		row1[i] = fmt.Sprintf("R1V%d", i+1)
		row2[i] = fmt.Sprintf("R2V%d", i+1)
	}
	join := func(cells []string) string { return "| " + strings.Join(cells, " | ") + " |" }
	return strings.Join([]string{
		join(headers),
		join(seps),
		join(row1),
		join(row2),
	}, "\n")
}

// TestTableSmoke sends three real rich-table messages to the test Telegram chat.
// It is guarded by TG_TABLE_SMOKE=1 so it never fires during normal go test ./...
func TestTableSmoke(t *testing.T) {
	if os.Getenv("TG_TABLE_SMOKE") == "" {
		t.Skip("set TG_TABLE_SMOKE=1 to run the real-send table smoke")
	}

	// Locate credentials file: TG_SMOKE_CONFIG_DIR env or ~/.tg-cli-test
	configDir := os.Getenv("TG_SMOKE_CONFIG_DIR")
	if configDir == "" {
		configDir = filepath.Join(os.Getenv("HOME"), ".tg-cli-test")
	}
	credPath := filepath.Join(configDir, "credentials.json")
	data, err := os.ReadFile(credPath)
	if err != nil {
		t.Skipf("cannot read test credentials at %s: %v", credPath, err)
	}
	var creds smokeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Skipf("cannot parse credentials JSON at %s: %v", credPath, err)
	}
	if creds.BotToken == "" {
		t.Skip("botToken is empty in test credentials — configure the test bot first")
	}
	if creds.PairingAllow.DefaultChatID == "" {
		t.Skip("pairingAllow.defaultChatId is empty in test credentials — pair the bot first")
	}

	// Create a real sending bot (no poller needed — sending is stateless via HTTP API)
	bot, err := tele.NewBot(tele.Settings{Token: creds.BotToken})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	chatIDInt, err := strconv.ParseInt(creds.PairingAllow.DefaultChatID, 10, 64)
	if err != nil {
		t.Fatalf("cannot parse defaultChatId %q as int64: %v", creds.PairingAllow.DefaultChatID, err)
	}
	chat := &tele.Chat{ID: chatIDInt}

	// Three smoke shapes: name, markdown source
	type smokeCase struct {
		name string
		md   string
	}

	normalMD := `# 🔬 T2.0 smoke [1/3] normal table

| Language | Paradigm       | Year |
| -------- | -------------- | ---- |
| Go       | Compiled       | 2009 |
| Python   | Interpreted    | 1991 |
| Rust     | Systems / Safe | 2010 |`

	wideMD := "# 🔬 T2.0 smoke [2/3] wide table (20 columns)\n\n" + buildWideTableMD()

	cjkMD := `# 🔬 T2.0 smoke [3/3] CJK + inline formatting

| 名称         | 描述                   | 状态          |
| ------------ | ---------------------- | ------------- |
| **加粗文本** | 这是加粗内容           | ` + "`已完成`" + ` |
| 普通文本     | 普通中文描述           | 进行中        |
| *斜体内容*   | 混合 CJK 与 formatting | 待处理        |`

	cases := []smokeCase{
		{name: "normal", md: normalMD},
		{name: "wide-20col", md: wideMD},
		{name: "cjk+bold", md: cjkMD},
	}

	for i, tc := range cases {
		html := markdown.RenderRichHTML(tc.md)

		// Capture stderr to detect RICH_FALLBACK (fallback = Telegram rejected the rich table)
		origStderr := os.Stderr
		pr, pw, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatalf("os.Pipe: %v", pipeErr)
		}
		os.Stderr = pw

		msg, sendErr := RetrySendRich(bot, chat, html, RichSendOpts{})

		pw.Close()
		os.Stderr = origStderr
		stderrBytes, _ := io.ReadAll(pr)
		stderrOutput := string(stderrBytes)

		if sendErr != nil {
			t.Errorf("smoke [%s] send error: %v", tc.name, sendErr)
			continue
		}
		if msg == nil || msg.ID == 0 {
			t.Errorf("smoke [%s] got nil or zero-ID message", tc.name)
			continue
		}
		if strings.Contains(stderrOutput, "RICH_FALLBACK") {
			t.Errorf("smoke [%s] triggered RICH_FALLBACK — Telegram rejected the rich table; stderr: %s", tc.name, stderrOutput)
			continue
		}
		t.Logf("smoke [%s] OK msg_id=%d", tc.name, msg.ID)

		// Avoid flood limit between sends (skip sleep after last)
		if i < len(cases)-1 {
			time.Sleep(1 * time.Second)
		}
	}
}
