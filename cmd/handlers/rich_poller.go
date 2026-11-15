package handlers

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"
)

// RichReplyPoller is telebot's LongPoller (same getUpdates request, same retry-on-error loop) with one addition:
// telebot v3.3.8 has no rich_message field, so a reply to a rich bot message arrives with ReplyTo.Text empty. The
// poller reads message.reply_to_message.rich_message from the raw update and splits its plain text into tg-cli's
// header (the blocks up to the first details block, which holds the 📟 line) and the body: the body goes into
// ReplyTo.Text, which the reply quote reads, the header into ReplyTo.Payload (telebot sets Payload only on the
// main message, so a reply never has one). extractReplyTarget looks for the 📟 line in both.
type RichReplyPoller struct {
	Timeout      time.Duration
	LastUpdateID int
}

func (p *RichReplyPoller) Poll(b *tele.Bot, dest chan tele.Update, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		data, err := b.Raw("getUpdates", map[string]string{
			"offset":          strconv.Itoa(p.LastUpdateID + 1),
			"timeout":         strconv.Itoa(int(p.Timeout / time.Second)),
			"allowed_updates": "null",
		})
		if err != nil {
			continue
		}
		updates, err := decodeUpdates(data)
		if err != nil {
			continue
		}
		for _, update := range updates {
			p.LastUpdateID = update.ID
			dest <- update
		}
	}
}

// decodeUpdates decodes a getUpdates response and fills ReplyTo.Payload and ReplyTo.Text from reply_to_message.rich_message.
func decodeUpdates(data []byte) ([]tele.Update, error) {
	var resp struct{ Result []json.RawMessage }
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	updates := make([]tele.Update, len(resp.Result))
	for i, raw := range resp.Result {
		if err := json.Unmarshal(raw, &updates[i]); err != nil {
			return nil, err
		}
		var rich struct {
			Message struct {
				ReplyTo struct {
					RichMessage struct{ Blocks []any } `json:"rich_message"`
				} `json:"reply_to_message"`
			}
		}
		if err := json.Unmarshal(raw, &rich); err != nil {
			return nil, err
		}
		if msg := updates[i].Message; msg != nil && msg.ReplyTo != nil && msg.ReplyTo.Text == "" {
			msg.ReplyTo.Payload, msg.ReplyTo.Text = richHeaderBody(rich.Message.ReplyTo.RichMessage.Blocks)
		}
	}
	return updates, nil
}

// richHeaderBody splits the blocks of a rich message after the first details block: tg-cli puts its header
// (first line, then the details block with the 📂 👤 🏷 📟 🖥 📊 lines) in front of the content. Without a details
// block the whole message is body.
func richHeaderBody(blocks []any) (header, body string) {
	for i, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["type"] == "details" {
			return richBlocksText(blocks[:i+1]), richBlocksText(blocks[i+1:])
		}
	}
	return "", richBlocksText(blocks)
}

// richBlocksText flattens RichBlock values to plain text, one line per block.
func richBlocksText(blocks []any) string {
	lines := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if line := richText(b); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// richText flattens a RichText (a string, an array of RichText, or a typed object) or a RichBlock to plain text:
// a list item starts with its label ("1.", "•") and a table row is its cells joined by " | ".
func richText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var sb strings.Builder
		for _, e := range x {
			sb.WriteString(richText(e))
		}
		return sb.String()
	case map[string]any:
		var parts []string
		for _, k := range []string{"summary", "text"} {
			if s := richText(x[k]); s != "" {
				parts = append(parts, s)
			}
		}
		for _, k := range []string{"blocks", "items"} {
			if blocks, ok := x[k].([]any); ok {
				if s := richBlocksText(blocks); s != "" {
					parts = append(parts, s)
				}
			}
		}
		rows, _ := x["cells"].([]any)
		for _, row := range rows {
			var cells []string
			cs, _ := row.([]any)
			for _, c := range cs {
				cells = append(cells, richText(c))
			}
			parts = append(parts, strings.Join(cells, " | "))
		}
		s := strings.Join(parts, "\n")
		if label, _ := x["label"].(string); label != "" {
			s = label + " " + s
		}
		return s
	}
	return ""
}
