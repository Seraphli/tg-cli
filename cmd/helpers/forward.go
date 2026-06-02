package helpers

import (
	"fmt"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// BuildAtHeader builds the header line for an @ channel message.
func BuildAtHeader(from, to string) string {
	return fmt.Sprintf("🔗 [@] `%s` → `%s`", from, to)
}

// BuildAtMsg assembles a full @ channel message from header, instructions, and content.
func BuildAtMsg(from, to, instructions, content string) string {
	header := BuildAtHeader(from, to)
	if instructions == "" && content == "" {
		return header
	}
	if content == "" {
		return header + "\n---\n" + instructions
	}
	if instructions == "" {
		return header + "\n---\n" + content
	}
	return header + "\n---\n" + instructions + "\n---\n" + content
}

// SendPagedForward sends a forwarded @ channel message as a single paginated TG message.
// If the body fits in one chunk (after accounting for header), sends directly with no buttons.
// If multi-chunk, sends the first page with pagination + collapse button, stores in pages cache.
func SendPagedForward(bot *tele.Bot, chat *tele.Chat, header, body string, pages *stores.PageCacheStore, sessionID string, opts ...interface{}) error {
	cfg, _ := config.LoadAppConfig()
	paginationMax := 4000
	if cfg.PaginationMaxRunes > 0 {
		paginationMax = cfg.PaginationMaxRunes
	}
	maxBody := paginationMax - len([]rune(header)) - 100
	if maxBody < 500 {
		maxBody = 500
	}
	chunks := SplitBody(body, maxBody)
	collapseMk := &tele.ReplyMarkup{}
	collapseBtn := collapseMk.Data("📗 收起", "ce", "c")
	if len(chunks) <= 1 {
		text := header + body
		kb := &tele.ReplyMarkup{}
		btn := kb.Data("📗 收起", "ce", "c")
		kb.Inline(tele.Row{btn})
		sendOpts := append([]interface{}{kb}, opts...)
		sent, err := RetrySend(bot, chat, text, sendOpts...)
		if err != nil {
			return err
		}
		pages.Store(sent.ID, sessionID, &stores.PageEntry{
			Chunks:  []string{body},
			Header:  header,
			RawMode: true,
		})
		return nil
	}
	extraRows := []tele.Row{{collapseBtn}}
	kb := BuildPageKeyboardWithExtra(1, len(chunks), extraRows)
	text := header + chunks[0]
	sendOpts := append([]interface{}{kb}, opts...)
	sent, err := RetrySend(bot, chat, text, sendOpts...)
	if err != nil {
		return err
	}
	pages.Store(sent.ID, sessionID, &stores.PageEntry{
		Chunks:  chunks,
		Header:  header,
		RawMode: true,
	})
	logger.Info(fmt.Sprintf("SendPagedForward: %d pages, msg_id=%d", len(chunks), sent.ID))
	return nil
}
