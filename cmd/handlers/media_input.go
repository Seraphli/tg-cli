package handlers

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// albumWait is how long an album waits for its next item before it counts as complete; downloadBackoff are the waits
// before the retries of a failed download. Variables so that a test can shorten them.
var (
	albumWait       = 1500 * time.Millisecond
	downloadBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
)

// mediaItem is one picture or document message: the file to download and the context its message arrived in.
type mediaItem struct {
	ctx      tele.Context
	fileID   string
	fileName string // the name the file is saved under in uploadsDir
	label    string // how the failure lines name a document (its file name); empty for a photo
}

// failedFile is a file that did not come through, as the reply and its retry button know it.
type failedFile struct {
	item    mediaItem
	label   string
	reason  string // the cause only: the request URL of a telebot error holds the bot token
	network bool   // a network error, which the retry button covers (an error Telegram returns on purpose is final)
	sent    bool   // a retry got it through
}

// retryEntry is what the retry button of one "Download failed" reply needs. It lives in memory only (callback data is
// limited to 64 bytes, and file ids are longer): the first item's message routes the injection like the original did.
type retryEntry struct {
	mu    sync.Mutex // held while a tap is served: a second tap meanwhile is dropped
	first *tele.Message
	files []failedFile
}

var (
	albumMu      sync.Mutex
	albumItems   = map[string][]mediaItem{} // the items of the albums still collecting, by "<chat id>|<AlbumID>"
	retryEntries sync.Map                   // "<chat id>|<reply message id>" -> *retryEntry
)

func retryKey(chatID int64, msgID int) string { return fmt.Sprintf("%d|%d", chatID, msgID) }

// collectMedia takes one picture or document message. Without an AlbumID it is processed at once, as a group of one.
// An item of an album is kept and the album is processed albumWait after its LAST item (every item starts a timer, only
// the timer of the newest item finds the count unchanged); no handler goroutine waits.
func collectMedia(bs *types.BotState, it mediaItem) error {
	albumID := it.ctx.Message().AlbumID
	if albumID == "" {
		return processMedia(bs, []mediaItem{it})
	}
	key := fmt.Sprintf("%d|%s", it.ctx.Chat().ID, albumID)
	albumMu.Lock()
	defer albumMu.Unlock()
	albumItems[key] = append(albumItems[key], it)
	n := len(albumItems[key])
	time.AfterFunc(albumWait, func() {
		albumMu.Lock()
		items := albumItems[key]
		if len(items) != n { // a later item restarted the wait
			albumMu.Unlock()
			return
		}
		delete(albumItems, key)
		albumMu.Unlock()
		if err := processMedia(bs, items); err != nil {
			logger.Error(fmt.Sprintf("Album %s: %s", key, downloadReason(err)))
		}
	})
	return nil
}

// downloadAll downloads the files of items in parallel, each with the retries of downloadWithRetry; the results follow items.
func downloadAll(bot *tele.Bot, items []mediaItem) ([]string, []error) {
	paths := make([]string, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i], errs[i] = downloadWithRetry(bot, it.fileID, it.fileName)
		}()
	}
	wg.Wait()
	return paths, errs
}

// downloadWithRetry downloads a Telegram file; a network error is tried again after each wait of downloadBackoff (1, 2 and 4 s), an
// error Telegram returns on purpose (helpers.IsNonRetryable: a 4xx other than 429) is returned at once.
func downloadWithRetry(bot *tele.Bot, fileID, fileName string) (string, error) {
	path, err := downloadTGFile(bot, fileID, fileName)
	for _, wait := range downloadBackoff {
		if err == nil || helpers.IsNonRetryable(err) {
			break
		}
		time.Sleep(wait)
		path, err = downloadTGFile(bot, fileID, fileName)
	}
	return path, err
}

// downloadReason is the cause of a failed Telegram request (a download, or a reply or send of the media intake) without the
// request URL: the url.Error of a transport failure holds the URL and so the bot token, its inner error ('EOF') does not;
// any other error (a Telegram answer) has no URL.
func downloadReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}

// processMedia downloads the files of one message (one item, or the items of a complete album) and injects them as ONE
// message: the paths in message id order, then the captions in message id order joined by "\n", then one line per file that
// failed. Routing, reply target and reaction are those of the item with the lowest message id (processUserInput on its
// context). Nothing is injected when no file came through and there is no caption. A failure is answered in Telegram.
func processMedia(bs *types.BotState, items []mediaItem) error {
	sort.Slice(items, func(i, j int) bool { return items[i].ctx.Message().ID < items[j].ctx.Message().ID })
	downloaded, errs := downloadAll(bs.Bot, items)
	var paths, captions, lines []string
	var failed []failedFile
	for i, it := range items {
		if caption := it.ctx.Message().Caption; caption != "" {
			captions = append(captions, caption)
		}
		if errs[i] == nil {
			paths = append(paths, downloaded[i])
			continue
		}
		f := failedFile{item: it, label: it.label, reason: downloadReason(errs[i]), network: !helpers.IsNonRetryable(errs[i])}
		if f.label == "" {
			f.label = "photo"
			if len(items) > 1 {
				f.label = fmt.Sprintf("photo %d of %d", i+1, len(items))
			}
		}
		logger.Error(fmt.Sprintf("Download failed: msg_id=%d %s: %s", it.ctx.Message().ID, f.label, f.reason))
		failed = append(failed, f)
		lines = append(lines, fmt.Sprintf("[download failed] %s: %s", f.label, f.reason))
	}
	injected := len(paths) > 0 || len(captions) > 0
	var err error
	if injected {
		err = processUserInput(bs, items[0].ctx, bs.Bot, strings.Join(append(captions, lines...), "\n"), false, "", paths...)
	}
	if len(failed) > 0 {
		replyDownloadFailed(bs.Bot, items[0], failed, injected)
	}
	return err
}

// retryHint is the sentence that points at the retry button: "Tap 🔁 to try again." when every file still failing failed on the
// network, "Tap 🔁 to try the network failures again." when a refused file is among them, "" when no network failure is left.
func retryHint(files []failedFile) string {
	network, failures := 0, 0
	for _, f := range files {
		if f.sent {
			continue
		}
		failures++
		if f.network {
			network++
		}
	}
	switch network {
	case 0:
		return ""
	case failures:
		return "Tap 🔁 to try again."
	}
	return "Tap 🔁 to try the network failures again."
}

// replyDownloadFailed answers the first item of a message with failed downloads: one line per file with its cause, what
// became of the rest of the message, and the retry button when a network failure is among them. injected tells whether a
// path or a caption went into the session.
func replyDownloadFailed(bot *tele.Bot, first mediaItem, failed []failedFile, injected bool) {
	lines := make([]string, len(failed))
	for i, f := range failed {
		lines[i] = "• " + f.label + ": " + f.reason
	}
	rest := "Nothing was sent to the session."
	if injected {
		rest = "The rest of the message went in."
	}
	text := "❌ Download failed:\n" + strings.Join(lines, "\n") + "\n" + rest
	var opts []interface{}
	hint := retryHint(failed)
	if hint != "" {
		text += " " + hint
		menu := &tele.ReplyMarkup{}
		menu.Inline(menu.Row(menu.Data("🔁 Retry download", "dlretry")))
		opts = append(opts, menu)
	}
	sent, err := bot.Reply(first.ctx.Message(), text, opts...)
	if err != nil {
		logger.Error(fmt.Sprintf("Download failed reply: %s", downloadReason(err)))
		return
	}
	if hint != "" {
		retryEntries.Store(retryKey(first.ctx.Chat().ID, sent.ID), &retryEntry{first: first.ctx.Message(), files: failed})
	}
}

// registerMediaCallbacks registers the "dlretry" button of the "Download failed" reply. A tap downloads every network-failed
// file of that reply again, injects those that came through as ONE message (routed like the original: processUserInput on a
// copy of the first item's message) with a line that says they failed before, and edits the reply. The entry lives in
// memory: after a restart the button says that it expired.
func registerMediaCallbacks(bs *types.BotState) {
	bot := bs.Bot
	bot.Handle(&tele.InlineButton{Unique: "dlretry"}, func(c tele.Context) error {
		c.Respond()
		msg := c.Callback().Message
		v, ok := retryEntries.Load(retryKey(msg.Chat.ID, msg.ID))
		if !ok {
			_, err := helpers.RetryEdit(bot, msg, msg.Text+"\n⚠️ This retry button expired when the bot restarted. Please send the file again.", &tele.ReplyMarkup{})
			return err
		}
		e := v.(*retryEntry)
		if !e.mu.TryLock() {
			return nil
		}
		defer e.mu.Unlock()
		var todo []mediaItem
		var at []int
		for i, f := range e.files {
			if f.network && !f.sent {
				todo = append(todo, f.item)
				at = append(at, i)
			}
		}
		downloaded, errs := downloadAll(bot, todo)
		var paths, labels []string
		for j, i := range at {
			f := &e.files[i]
			if errs[j] != nil {
				f.reason, f.network = downloadReason(errs[j]), !helpers.IsNonRetryable(errs[j])
				continue
			}
			f.sent = true
			paths = append(paths, downloaded[j])
			labels = append(labels, f.label)
		}
		var err error
		if len(paths) > 0 {
			m := *e.first
			err = processUserInput(bs, bot.NewContext(tele.Update{Message: &m}), bot, "[failed to download earlier, retried now] "+strings.Join(labels, ", "), false, "", paths...)
		}
		header, lines := "✅ Download retry:", make([]string, len(e.files))
		for i, f := range e.files {
			lines[i] = "• " + f.label + ": " + f.reason
			if f.sent {
				lines[i] = "• " + f.label + ": sent to the session"
			} else {
				header = "⚠️ Download retry:"
			}
		}
		text, markup := header+"\n"+strings.Join(lines, "\n"), &tele.ReplyMarkup{}
		if hint := retryHint(e.files); hint != "" {
			text += "\n" + hint
			markup.Inline(markup.Row(markup.Data("🔁 Retry download", "dlretry")))
		}
		helpers.RetryEdit(bot, msg, text, markup)
		return err
	})
}
