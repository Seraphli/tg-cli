package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/tgfake"
	tele "gopkg.in/telebot.v3"
)

// paste is what the recording pane of mediaEnv holds for one bracketed paste; ctrlU for the clear that opens an injection.
func paste(text string) string { return "^[[200~" + text + "^[[201~" }

const ctrlU = "^U"

// eventually polls cond every 50 ms for up to 20 s and reports whether it held.
func eventually(cond func() bool) bool {
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return false
}

// mediaEnv is a topicRootEnv whose pane records every byte it receives in rec (ESC as ^[, a bracketed paste as
// ^[[200~...^[[201~, Ctrl-U as ^U, Enter as a newline) and shows them after a "❯ " prompt that comes back after every Enter,
// without echo or line buffering, so the pane scan of SafeInjectText confirms the first paste of every injection as it does
// on a real prompt. Its fake Bot API serves one file per file id whose body holds the id; uploads go to a temp dir and the
// retry waits are 10 ms.
type mediaEnv struct {
	*topicRootEnv
	rec string
}

// newMediaEnv builds the env; getFile may answer a getFile request itself (nil: the normal answer), to delay or drop it.
func newMediaEnv(t *testing.T, getFile func(id string) *tgfake.Reply) *mediaEnv {
	t.Helper()
	e := &mediaEnv{topicRootEnv: newTopicRootEnv(t), rec: filepath.Join(t.TempDir(), "rec")}
	origDir, origBackoff := uploadsDir, downloadBackoff
	uploadsDir, downloadBackoff = t.TempDir(), []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { uploadsDir, downloadBackoff = origDir, origBackoff })
	e.s.On("getFile", func(p map[string]interface{}) tgfake.Reply {
		id := p["file_id"].(string)
		if r := getFile(id); r != nil {
			return *r
		}
		e.s.On(id, func(map[string]interface{}) tgfake.Reply { return tgfake.Reply{Result: id} }) // the file: {"ok":true,"result":"<id>"}
		return tgfake.Reply{Result: map[string]interface{}{"file_id": id, "file_path": id}}
	})
	recorder := fmt.Sprintf(`printf '\033[?2004h'; stty -icanon -echo; cat -v | tee %s | perl -e '$|=1; print "❯ "; while (sysread(STDIN, $c, 1)) { print $c; print "❯ " if $c eq "\n" }'`, e.rec)
	if err := injector.SendKeys(injector.TmuxTarget{PaneID: e.pane}, recorder, "Enter"); err != nil {
		t.Fatalf("start recorder: %v", err)
	}
	if !eventually(func() bool { _, err := os.Stat(e.rec); return err == nil }) {
		t.Fatal("the recorder did not start")
	}
	return e
}

// expectPasted waits until the pane has received exactly want.
func (e *mediaEnv) expectPasted(t *testing.T, want string) {
	t.Helper()
	var got string
	if !eventually(func() bool { b, _ := os.ReadFile(e.rec); got = string(b); return got == want }) {
		t.Fatalf("the pane received %q, want %q", got, want)
	}
}

// calls returns the requests the bot made to one Bot API method, in order.
func (e *mediaEnv) calls(method string) []tgfake.Call {
	var out []tgfake.Call
	for _, c := range e.s.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// saved is the path of the downloaded file whose body holds id.
func saved(t *testing.T, id string) string {
	t.Helper()
	var path string
	if !eventually(func() bool {
		files, _ := filepath.Glob(filepath.Join(uploadsDir, "*"))
		for _, f := range files {
			if body, _ := os.ReadFile(f); strings.Contains(string(body), `"`+id+`"`) {
				path = f
			}
		}
		return path != ""
	}) {
		t.Fatalf("no downloaded file holds %s", id)
	}
	return path
}

// buttons returns the callback_data of the inline buttons of a request (telebot sends reply_markup as a JSON string).
func buttons(t *testing.T, c tgfake.Call) []string {
	t.Helper()
	raw, _ := c.Params["reply_markup"].(string)
	var markup struct {
		InlineKeyboard [][]struct {
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(raw), &markup); err != nil {
		t.Fatalf("decode reply_markup %q: %v", raw, err)
	}
	var out []string
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			out = append(out, b.CallbackData)
		}
	}
	return out
}

// photoMsg is a photo message typed in the bound topic; its file id is its message id; albumID "" means no album.
func photoMsg(id int, albumID, caption string) *tele.Message {
	m := topicRootMsg(id, "")
	m.Photo, m.AlbumID, m.Caption = &tele.Photo{File: tele.File{FileID: fmt.Sprint(id)}}, albumID, caption
	return m
}

// TestAlbum_TwoPicturesOneMessageInIdOrder is the 2026-11-23 incident: the two pictures of an album reach the bot in the
// wrong order and the lower message id downloads last. The agent must get both pictures, then the caption, as ONE message
// in message id order.
func TestAlbum_TwoPicturesOneMessageInIdOrder(t *testing.T) {
	e := newMediaEnv(t, func(id string) *tgfake.Reply {
		if id == "581448" {
			time.Sleep(800 * time.Millisecond)
		}
		return nil
	})
	e.bot.ProcessUpdate(tele.Update{Message: photoMsg(581449, "alb1", "")})
	e.bot.ProcessUpdate(tele.Update{Message: photoMsg(581448, "alb1", "what is wrong here")})
	e.expectPasted(t, ctrlU+paste(saved(t, "581448"))+paste(" "+saved(t, "581449"))+paste("\nwhat is wrong here")+"\n")
	if n := len(e.calls("sendMessage")); n != 0 {
		t.Errorf("the bot sent %d replies, want none", n)
	}
}

// TestDownloadRetry_GetFileEOFThenPictureAndCaptionInjected is the 2026-11-03 incident: getFile of a picture with a caption
// ends in EOF once. The download is tried again, the agent gets the picture and the caption, and no failure is reported.
func TestDownloadRetry_GetFileEOFThenPictureAndCaptionInjected(t *testing.T) {
	var tries atomic.Int32
	e := newMediaEnv(t, func(string) *tgfake.Reply {
		if tries.Add(1) == 1 {
			return &tgfake.Reply{Drop: true}
		}
		return nil
	})
	e.bot.ProcessUpdate(tele.Update{Message: photoMsg(546771, "", "what is wrong here")})
	e.expectPasted(t, ctrlU+paste(saved(t, "546771"))+paste("\nwhat is wrong here")+"\n")
	if n := len(e.calls("sendMessage")); n != 0 {
		t.Errorf("the bot sent %d replies, want none", n)
	}
}

// TestRetryButton_ThirdFailureThenTapInjectsFile: the network stays down for all four tries. The caption still reaches
// the agent with a line for the failed picture, the bot replies with ONE retry button, and after the network is back a tap
// downloads the picture again, injects it, and edits the reply so that it has no button.
func TestRetryButton_ThirdFailureThenTapInjectsFile(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	e := newMediaEnv(t, func(string) *tgfake.Reply {
		if down.Load() {
			return &tgfake.Reply{Drop: true}
		}
		return nil
	})
	e.bot.ProcessUpdate(tele.Update{Message: photoMsg(546771, "", "what is wrong here")})
	first := ctrlU + paste("what is wrong here\n[download failed] photo: EOF") + "\n"
	e.expectPasted(t, first)
	replies := e.calls("sendMessage")
	if len(replies) != 1 || len(buttons(t, replies[0])) != 1 {
		t.Fatalf("the bot sent %d replies, want one with exactly one button", len(replies))
	}
	down.Store(false)
	reply := &tele.Message{ID: 9001, Chat: forumChat, Text: replies[0].Params["text"].(string)} // 9001: the fake's message id for a sent message
	e.bot.ProcessUpdate(tele.Update{Callback: &tele.Callback{Sender: &tele.User{ID: 1}, Message: reply, Data: buttons(t, replies[0])[0]}})
	e.expectPasted(t, first+ctrlU+paste(saved(t, "546771"))+paste("\n[failed to download earlier, retried now] photo")+"\n")
	if edits := e.calls("editMessageText"); len(edits) != 1 || len(buttons(t, edits[0])) != 0 {
		t.Errorf("the reply was edited %d times, want once and without a button", len(edits))
	}
}
