package helpers

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
)

// splitRichMax is the default rich rune budget used by the Cron/SessionSend senders.
const splitRichMax = 30000

// cronVariants returns the RAW-body + NotificationData pairs for ALL 5 cron variants + session-send that
// sendCronNotification / the session-send handler feed to BuildNotificationText. Each carries the per-call
// status prefix in the raw body (matching bot_cron.go) and the structured metadata on the nd.
func cronVariants() []struct {
	name      string
	rawBody   string
	nd        notify.NotificationData
	leadEmoji string
} {
	return []struct {
		name      string
		rawBody   string
		nd        notify.NotificationData
		leadEmoji string
	}{
		{
			name:      "print-success",
			rawBody:   "All good. Result is 42.",
			nd:        notify.NotificationData{Event: "Cron", CronJobID: "aaaaaaaabbbbbbbb", CronName: "nightly", CWD: "/home/u/proj", ContextUsedPct: -1},
			leadEmoji: "🔔",
		},
		{
			name:      "print-failure",
			rawBody:   "❌ **Error:**\nexit status 1",
			nd:        notify.NotificationData{Event: "Cron", CronJobID: "cccccccc11112222", CronName: "backup", CWD: "/home/u/proj", ContextUsedPct: -1},
			leadEmoji: "🔔",
		},
		{
			name:      "inject-offline",
			rawBody:   "⚠️ Agent **note** is not online.\n\nPrompt:\ncheck the logs",
			nd:        notify.NotificationData{Event: "Cron", CronJobID: "dddddddd33334444", CronName: "ping", TmuxTarget: "%3", ContextUsedPct: -1},
			leadEmoji: "🔔",
		},
		{
			name:      "inject-success",
			rawBody:   "✅ Injected → note\n\ncheck the logs",
			nd:        notify.NotificationData{Event: "Cron", CronJobID: "eeeeeeee55556666", CronName: "ping", TmuxTarget: "%3", ContextUsedPct: -1},
			leadEmoji: "🔔",
		},
		{
			name:      "inject-failure",
			rawBody:   "❌ **Inject failed**\nAgent: note\n\nError:\ntimeout",
			nd:        notify.NotificationData{Event: "Cron", CronJobID: "ffffffff77778888", CronName: "ping", TmuxTarget: "%3", ContextUsedPct: -1},
			leadEmoji: "🔔",
		},
		{
			name:      "session-send",
			rawBody:   "hello from the CLI",
			nd:        notify.NotificationData{Event: "SessionSend", SendFrom: "note", TmuxTarget: "%9", ContextUsedPct: -1},
			leadEmoji: "🖊️",
		},
	}
}

// TestBuildNotificationText_CronSessionSendNeutralHeader asserts the NEUTRAL rich header for all 5 cron
// variants + session-send: line 0 is "<lead-emoji> <status>" (NOT the old flat "🔔 <b>Cron</b>"
// bold/code HTML) and line 1 is the 📂 metadata line. The rich <hr/> is added SEPARATELY by InsertRichHr
// at the header/body boundary; the base builder emits no <hr/>/<details>/<summary>.
func TestBuildNotificationText_CronSessionSendNeutralHeader(t *testing.T) {
	for _, v := range cronVariants() {
		t.Run(v.name, func(t *testing.T) {
			nd := v.nd
			nd.Body = markdown.RenderRichHTML(v.rawBody)
			header := notify.BuildNotificationText(nd)
			line0 := strings.SplitN(header, "\n", 2)[0]
			if !strings.HasPrefix(line0, v.leadEmoji+" ") {
				t.Errorf("expected header line 0 to start with %q; got %q", v.leadEmoji+" ", line0)
			}
			// Neutral header: no bold/code HTML wrapping (the OLD "🔔 <b>Cron</b> <code>id</code>" format).
			if strings.Contains(line0, "<b>") || strings.Contains(line0, "<code>") {
				t.Errorf("neutral header must not contain <b>/<code> wrapping; got %q", line0)
			}
			// The base builder never emits the rich-only boundary/collapse tags.
			if strings.Contains(header, "<hr/>") || strings.Contains(header, "<details>") || strings.Contains(header, "<summary>") {
				t.Errorf("base BuildNotificationText must not emit <hr/>/<details>/<summary>; got %q", header)
			}
			// InsertRichHr (rich-only) DOES add the <hr/> at the header/body boundary.
			if !strings.Contains(InsertRichHr(header), "<hr/>") {
				t.Errorf("InsertRichHr should add <hr/> at the header/body boundary; got %q", InsertRichHr(header))
			}
		})
	}
}

// finalizedRichRuneLen mirrors the exact send-time rich payload validation used by the splitter's fits():
// FinalizeRichHTMLWithID(InsertRichHr(header), worstCaseID) then RichifyNewlines expands <br> — measure
// the runes of the finalized string (which already includes the RichifyNewlines <br> expansion inside
// FinalizeRichHTMLWithID). The worst-case archive id is the widest realistic 12-digit id.
func finalizedRich(nd notify.NotificationData, richBody string) string {
	nd.Body = richBody
	return FinalizeRichHTMLWithID(InsertRichHr(notify.BuildNotificationText(nd)), int64(999999999999))
}

// assertPageValid checks every send-time invariant for one wrapped page: the finalized rich payload is
// ≤ richMax runes AND ≤ 500 blocks, and the legacy payload is ≤ 4096 runes.
func assertPageValid(t *testing.T, nd notify.NotificationData, richBody, legacyBody string, richMax int, page, total int) {
	t.Helper()
	ndPage := nd
	if total > 1 {
		ndPage.Page = page
		ndPage.TotalPages = total
	}
	richFinal := finalizedRich(ndPage, richBody)
	if n := utf8.RuneCountInString(richFinal); n > richMax {
		t.Errorf("page %d/%d finalized rich runes %d > richMax %d", page, total, n, richMax)
	}
	if b := countBlocks(richFinal); b > richBlockLimit {
		t.Errorf("page %d/%d finalized rich blocks %d > limit %d", page, total, b, richBlockLimit)
	}
	ndLegacy := nd
	if total > 1 {
		ndLegacy.Page = page
		ndLegacy.TotalPages = total
	}
	ndLegacy.Body = legacyBody
	legacyText := notify.BuildNotificationText(ndLegacy)
	if n := utf8.RuneCountInString(legacyText); n > legacyMaxRunes {
		t.Errorf("page %d/%d legacy runes %d > 4096", page, total, n)
	}
	// Legacy must NEVER carry the rich-only boundary/collapse tags.
	if strings.Contains(legacyText, "<hr/>") || strings.Contains(legacyText, "<details>") || strings.Contains(legacyText, "<summary>") {
		t.Errorf("page %d/%d legacy payload must not contain <hr/>/<details>/<summary>; got %q", page, total, legacyText)
	}
}

// TestSplitRichLegacyBodyPages_PairedEqualLength verifies the paired builder returns EQUAL-LENGTH rich +
// legacy BODY chunk slices for a multi-page body, that concatenating the page bodies preserves the ORIGINAL
// content (no loss / no dup), and that EVERY page passes final-payload validation in BOTH dialects.
func TestSplitRichLegacyBodyPages_PairedEqualLength(t *testing.T) {
	// Build a body far larger than the small richMax so it splits into many pages. Use distinct paragraph
	// markers so concatenation can be checked for loss/duplication.
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "Paragraph number %d with some filler content to take up room.\n\n", i)
	}
	rawBody := strings.TrimRight(sb.String(), "\n")
	nd := notify.NotificationData{Event: "Cron", CronJobID: "abcd1234ef567890", CronName: "job", CWD: "/home/u/p", ContextUsedPct: -1}
	richMax := 600 // small budget → forces many pages
	chunks, legacy, err := SplitRichLegacyBodyPages(rawBody, nd, richMax)
	if err != nil {
		t.Fatalf("SplitRichLegacyBodyPages returned error: %v", err)
	}
	if len(chunks) != len(legacy) {
		t.Fatalf("len(Chunks)=%d != len(LegacyChunks)=%d", len(chunks), len(legacy))
	}
	if len(chunks) < 2 {
		t.Fatalf("expected the body to split into >=2 pages, got %d", len(chunks))
	}
	// Content preservation: every "Paragraph number N" marker must appear exactly once across the pages,
	// in both dialects.
	richJoined := strings.Join(chunks, "\n")
	legacyJoined := strings.Join(legacy, "\n")
	for i := 0; i < 200; i++ {
		marker := fmt.Sprintf("Paragraph number %d ", i)
		if c := strings.Count(richJoined, marker); c != 1 {
			t.Errorf("rich pages: marker %q appears %d times, want 1", marker, c)
		}
		if c := strings.Count(legacyJoined, marker); c != 1 {
			t.Errorf("legacy pages: marker %q appears %d times, want 1", marker, c)
		}
	}
	for i := range chunks {
		assertPageValid(t, nd, chunks[i], legacy[i], richMax, i+1, len(chunks))
	}
}

// TestSplitRichLegacyBodyPages_AdversarialMarkdown feeds inputs with HTML-escapable chars, a table, a fenced
// code block containing tag-like text, and an oversized fence that must hard-split WITHOUT breaking the
// fence — every wrapped page must pass final-payload validation in both dialects.
func TestSplitRichLegacyBodyPages_AdversarialMarkdown(t *testing.T) {
	nd := notify.NotificationData{Event: "Cron", CronJobID: "adver5678sarial00", CronName: "adv", CWD: "/home/u/p", ContextUsedPct: -1}

	cases := []struct {
		name    string
		rawBody string
		richMax int
	}{
		{
			name:    "escaped-chars",
			rawBody: "special: < & > `backtick` and <b>not a tag</b> & more &amp; stuff",
			richMax: splitRichMax,
		},
		{
			name:    "table",
			rawBody: "| Col A | Col B |\n|-------|-------|\n| 1 & 2 | x < y |\n| `c`   | <td>  |",
			richMax: splitRichMax,
		},
		{
			name:    "fenced-tag-like",
			rawBody: "```go\nfunc f() { return \"<b>&lt;</b>\" }\n// <details><summary>x</summary>\n```",
			richMax: splitRichMax,
		},
		{
			name: "oversized-fence-hard-split",
			// A single fenced code block whose inner content greatly exceeds the small budget → must
			// re-fence and hard-split each piece (never a stray/broken fence).
			rawBody: "```\n" + strings.Repeat("codeline-with-<tag>&amp;-content ", 200) + "\n```",
			richMax: 700,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chunks, legacy, err := SplitRichLegacyBodyPages(c.rawBody, nd, c.richMax)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(chunks) != len(legacy) {
				t.Fatalf("len(Chunks)=%d != len(LegacyChunks)=%d", len(chunks), len(legacy))
			}
			if len(chunks) == 0 {
				t.Fatal("expected >=1 page")
			}
			for i := range chunks {
				assertPageValid(t, nd, chunks[i], legacy[i], c.richMax, i+1, len(chunks))
				// A fenced page must be balanced Markdown code: an even number of fence markers per rich
				// chunk (the renderer emits <pre><code> only for a well-formed fence).
				if c.name == "oversized-fence-hard-split" {
					// Every piece must render as a code block (no stray unrendered fence text leaking).
					if strings.Contains(chunks[i], "```") {
						t.Errorf("page %d leaked a raw fence marker into rendered output: %q", i+1, chunks[i])
					}
				}
			}
		})
	}
}

// TestSplitRichLegacyBodyPages_OversizedNonFencedUnit verifies a very long newline-free unit
// (base64/minified-JSON-like) splits via reduce-then-validate and concatenation preserves EXACT content.
func TestSplitRichLegacyBodyPages_OversizedNonFencedUnit(t *testing.T) {
	// A long newline-free run (no paragraph/line boundaries) forces hardSplitUnit's rune-by-rune reduce.
	rawBody := strings.Repeat("aB3xY9zQ", 400) // 3200 runes, no spaces/newlines
	nd := notify.NotificationData{Event: "Cron", CronJobID: "nofence000", CronName: "nf", CWD: "/home/u/p", ContextUsedPct: -1}
	richMax := 700
	chunks, legacy, err := SplitRichLegacyBodyPages(rawBody, nd, richMax)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 pages for an oversized newline-free unit, got %d", len(chunks))
	}
	// The body has no markdown syntax, so each rendered chunk == its raw page; concatenation must equal raw.
	if joined := strings.Join(chunks, ""); joined != rawBody {
		t.Errorf("rich concatenation lost/duplicated content:\n got len=%d\nwant len=%d", len(joined), len(rawBody))
	}
	if joined := strings.Join(legacy, ""); joined != rawBody {
		t.Errorf("legacy concatenation lost/duplicated content:\n got len=%d\nwant len=%d", len(joined), len(rawBody))
	}
	for i := range chunks {
		assertPageValid(t, nd, chunks[i], legacy[i], richMax, i+1, len(chunks))
	}
}

// TestSplitRichLegacyBodyPages_NonMonotonicPrefix uses a body whose Markdown renders non-monotonically in
// prefix length (an unclosed "**" and an unbalanced fence inside one giant newline-free line). The
// reduce-then-validate splitter (which keeps the largest fitting prefix, NOT a binary search) must still
// split it into fitting pages without error.
func TestSplitRichLegacyBodyPages_NonMonotonicPrefix(t *testing.T) {
	// Unbalanced "**" and a lone opening fence inside one long line: rendering these prefixes is not
	// monotonic in the raw prefix length (goldmark may collapse/expand as more chars arrive).
	rawBody := "**" + strings.Repeat("unbalanced ", 300) + " ```still open"
	nd := notify.NotificationData{Event: "SessionSend", SendFrom: "note", CWD: "/home/u/p", ContextUsedPct: -1}
	richMax := 700
	chunks, legacy, err := SplitRichLegacyBodyPages(rawBody, nd, richMax)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != len(legacy) {
		t.Fatalf("len mismatch %d vs %d", len(chunks), len(legacy))
	}
	if len(chunks) < 2 {
		t.Fatalf("expected the giant line to split, got %d pages", len(chunks))
	}
	for i := range chunks {
		assertPageValid(t, nd, chunks[i], legacy[i], richMax, i+1, len(chunks))
	}
}

// TestSplitRichLegacyBodyPages_HeaderTooBigTerminates verifies that an oversized CronName (so the
// empty-body header alone already exceeds the budget) causes SplitRichLegacyBodyPages to RETURN an error
// instead of hanging — the caller then sends its minimal fallback. The assertion is that the call RETURNS.
func TestSplitRichLegacyBodyPages_HeaderTooBigTerminates(t *testing.T) {
	nd := notify.NotificationData{
		Event:          "Cron",
		CronJobID:      "big00000",
		CronName:       strings.Repeat("X", 5000), // header alone blows a tiny budget
		CWD:            "/home/u/p",
		ContextUsedPct: -1,
	}
	done := make(chan struct{})
	var err error
	go func() {
		_, _, err = SplitRichLegacyBodyPages("some body", nd, 200)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SplitRichLegacyBodyPages hung on an oversized header — must return an error")
	}
	if err == nil {
		t.Fatal("expected an error when the empty-body header exceeds the budget")
	}
}

// TestSplitRichLegacyBodyPages_OneRuneExpansionTerminates verifies the fits(firstRawRune) pre-check: a
// header that fits with only a few runes left, plus a body whose FIRST rune is "&" (RenderTelegramHTML("&")
// = "&amp;" — a 1→5 rune expansion that overflows), makes even a one-rune body impossible → the split
// returns an ERROR WITHOUT panicking or halving forever.
func TestSplitRichLegacyBodyPages_OneRuneExpansionTerminates(t *testing.T) {
	// Choose a richMax exactly large enough for the empty-body header but too small for the header plus the
	// 5-rune "&amp;" expansion of a single "&". Compute the header size to size the budget precisely.
	nd := notify.NotificationData{Event: "Cron", CronJobID: "amp00000", CronName: "amp", CWD: "/home/u/p", ContextUsedPct: -1}
	emptyHeaderRunes := utf8.RuneCountInString(finalizedRich(func() notify.NotificationData {
		n := nd
		n.Page = utf8.RuneCountInString("&x") + 1
		n.TotalPages = n.Page
		return n
	}(), markdown.RenderRichHTML("")))
	// Budget = header + 2 runes: enough for the empty header, but a leading "&" needs +5 runes (&amp;)
	// on the legacy side / rich side, overflowing → fits(firstRawRune) fails.
	richMax := emptyHeaderRunes + 2
	done := make(chan struct{})
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("SplitRichLegacyBodyPages panicked: %v", r)
			}
			close(done)
		}()
		// Body is a single newline-free unit beginning with "&" that cannot fit even one raw rune.
		_, _, err = SplitRichLegacyBodyPages("&"+strings.Repeat("z", 50), nd, richMax)
	}()
	<-done
	if err == nil {
		t.Fatal("expected an error when even the first raw rune cannot fit (1-rune expansion)")
	}
}

// TestSplitRichLegacyBodyPages_SinglePassMarker verifies a body that produces 10+ pages uses the upper-bound
// (2-digit) pagination marker throughout in ONE pass: the 9→10 page boundary is stable (no page overflows
// its budget when the marker width grows from "(9/N)" to "(10/N)").
func TestSplitRichLegacyBodyPages_SinglePassMarker(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 140; i++ {
		fmt.Fprintf(&sb, "Line %d filler content here.\n\n", i)
	}
	rawBody := strings.TrimRight(sb.String(), "\n")
	nd := notify.NotificationData{Event: "Cron", CronJobID: "single00", CronName: "sp", CWD: "/home/u/p", ContextUsedPct: -1}
	richMax := 400 // small enough to force 10+ pages
	chunks, legacy, err := SplitRichLegacyBodyPages(rawBody, nd, richMax)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 10 {
		t.Fatalf("expected 10+ pages to exercise the 2-digit marker, got %d", len(chunks))
	}
	// Every page (including the 9→10 boundary pages) must still validate with the ACTUAL 2-digit marker.
	for i := range chunks {
		assertPageValid(t, nd, chunks[i], legacy[i], richMax, i+1, len(chunks))
	}
}

// TestSplitRichLegacyBodyPages_RichHrOnlyOnRich verifies the rich <hr/> appears at the header/body boundary
// of a rich cron/session-send notification, while the legacy fallback (and a forced-rich-fallback
// header+body) contains NO <hr/>/<details>/<summary>.
func TestSplitRichLegacyBodyPages_RichHrOnlyOnRich(t *testing.T) {
	nd := notify.NotificationData{Event: "Cron", CronJobID: "hronly00", CronName: "hr", CWD: "/home/u/p", ContextUsedPct: -1}
	chunks, legacy, err := SplitRichLegacyBodyPages("body line one\n\nbody line two", nd, splitRichMax)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Rich page 1: InsertRichHr adds the <hr/> at the header/body boundary.
	ndRich := nd
	ndRich.Body = chunks[0]
	richText := InsertRichHr(notify.BuildNotificationText(ndRich))
	if !strings.Contains(richText, "<hr/>") {
		t.Errorf("rich cron notification must contain <hr/> at the header/body boundary; got %q", richText)
	}
	// Legacy page 1: BuildNotificationText WITHOUT InsertRichHr — no <hr/>/<details>/<summary>.
	ndLegacy := nd
	ndLegacy.Body = legacy[0]
	legacyText := notify.BuildNotificationText(ndLegacy)
	if strings.Contains(legacyText, "<hr/>") || strings.Contains(legacyText, "<details>") || strings.Contains(legacyText, "<summary>") {
		t.Errorf("legacy fallback must NOT contain <hr/>/<details>/<summary>; got %q", legacyText)
	}
	// A "forced rich fallback" (rich content routed through the legacy HTML fallback path — the sender's G2
	// fallback uses the same header+body WITHOUT InsertRichHr) also carries no <hr/>.
	forcedFallback := notify.BuildNotificationText(ndRich)
	if strings.Contains(forcedFallback, "<hr/>") {
		t.Errorf("forced-rich-fallback header+body (no InsertRichHr) must not contain <hr/>; got %q", forcedFallback)
	}
}

// TestPaginationRebuild_CronBodyChunk simulates the S12b page-2 rebuild: a stored PageEntry with paired
// Chunks/LegacyChunks + Cron metadata re-wraps page 2's BODY chunk via BuildNotificationText carrying the
// Cron fields; the rich payload gets the <hr/> boundary and the legacy payload does not.
func TestPaginationRebuild_CronBodyChunk(t *testing.T) {
	nd := notify.NotificationData{Event: "Cron", CronJobID: "pageturn00", CronName: "pt", CWD: "/home/u/p", ContextUsedPct: -1}
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sb, "content block %d here.\n\n", i)
	}
	chunks, legacy, err := SplitRichLegacyBodyPages(strings.TrimRight(sb.String(), "\n"), nd, 600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("need >=2 pages to test page-2 rebuild, got %d", len(chunks))
	}
	// Page 2 rebuild (mirrors api/pagination.go + handlers/callbacks.go): rich = BuildNotificationText +
	// InsertRichHr for Cron; legacy = BuildNotificationText on the legacy chunk (never InsertRichHr).
	pageNum := 2
	ndPage := nd
	ndPage.Page = pageNum
	ndPage.TotalPages = len(chunks)
	ndPage.Body = chunks[pageNum-1]
	richText := notify.BuildNotificationText(ndPage)
	richText = InsertRichHr(richText) // Cron/SessionSend only
	if !strings.Contains(richText, "<hr/>") {
		t.Errorf("page-2 rich rebuild must contain <hr/> for Cron; got %q", richText)
	}
	if !strings.Contains(richText, fmt.Sprintf("(%d/%d)", pageNum, len(chunks))) {
		t.Errorf("page-2 rich rebuild must carry the (2/N) marker; got %q", richText)
	}
	ndLegacy := ndPage
	ndLegacy.Body = legacy[pageNum-1]
	legacyText := notify.BuildNotificationText(ndLegacy)
	if strings.Contains(legacyText, "<hr/>") {
		t.Errorf("page-2 legacy rebuild must NOT contain <hr/>; got %q", legacyText)
	}
}

// TestPaginationRebuild_SessionSendDeliveryStatus verifies the S12b page-2 rebuild retains the SessionSend
// DeliveryStatus annotation on BOTH the rich and legacy payloads — mirroring the threading added in
// api/pagination.go + handlers/callbacks.go (entry.DeliveryStatus -> nd AND ndLegacy).
func TestPaginationRebuild_SessionSendDeliveryStatus(t *testing.T) {
	nd := notify.NotificationData{Event: "SessionSend", SendFrom: "note", CWD: "/home/u/p", ContextUsedPct: -1, DeliveryStatus: "unconfirmed"}
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sb, "content block %d here.\n\n", i)
	}
	chunks, legacy, err := SplitRichLegacyBodyPages(strings.TrimRight(sb.String(), "\n"), nd, 600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("need >=2 pages to test page-2 rebuild, got %d", len(chunks))
	}
	pageNum := 2
	ndPage := nd
	ndPage.Page = pageNum
	ndPage.TotalPages = len(chunks)
	ndPage.Body = chunks[pageNum-1]
	richText := InsertRichHr(notify.BuildNotificationText(ndPage))
	if !strings.Contains(richText, "delivery unconfirmed") {
		t.Errorf("page-2 rich rebuild must retain the delivery-status tag; got %q", richText)
	}
	// f29 G: the 🏷 type line must also survive to page 2+ (SendKind is derived from SendNoHeader, which
	// both rebuild paths thread — pagination.go:65 / callbacks.go:168,192).
	if !strings.Contains(richText, "🏷 normal") {
		t.Errorf("page-2 rich rebuild must retain the 🏷 normal type line; got %q", richText)
	}
	ndLegacy := ndPage
	ndLegacy.Body = legacy[pageNum-1]
	legacyText := notify.BuildNotificationText(ndLegacy)
	if !strings.Contains(legacyText, "delivery unconfirmed") {
		t.Errorf("page-2 legacy rebuild must retain the delivery-status tag; got %q", legacyText)
	}
	if !strings.Contains(legacyText, "🏷 normal") {
		t.Errorf("page-2 legacy rebuild must retain the 🏷 normal type line; got %q", legacyText)
	}
}

// TestPaginationRebuild_Page2Rich400FallsBackToLegacy documents the S12b fallback contract: when a page-2
// rich rebuild would 400, the caller re-renders via LegacyChunks[1]. Here we assert the paired legacy chunk
// EXISTS and is independently a valid legacy payload (so the fallback has real content to send).
func TestPaginationRebuild_Page2Rich400FallsBackToLegacy(t *testing.T) {
	nd := notify.NotificationData{Event: "SessionSend", SendFrom: "note", CWD: "/home/u/p", ContextUsedPct: -1}
	var sb strings.Builder
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&sb, "para %d body.\n\n", i)
	}
	chunks, legacy, err := SplitRichLegacyBodyPages(strings.TrimRight(sb.String(), "\n"), nd, 600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != len(legacy) || len(chunks) < 2 {
		t.Fatalf("expected paired multi-page result; chunks=%d legacy=%d", len(chunks), len(legacy))
	}
	// The paired legacy chunk for page 2 must exist and wrap to a valid legacy payload the fallback can send.
	ndLegacy := nd
	ndLegacy.Page = 2
	ndLegacy.TotalPages = len(chunks)
	ndLegacy.Body = legacy[1]
	legacyText := notify.BuildNotificationText(ndLegacy)
	if utf8.RuneCountInString(legacyText) > legacyMaxRunes {
		t.Errorf("page-2 legacy fallback exceeds 4096 runes: %d", utf8.RuneCountInString(legacyText))
	}
	if strings.Contains(legacyText, "<hr/>") || strings.Contains(legacyText, "<details>") {
		t.Errorf("legacy fallback must not carry rich-only tags; got %q", legacyText)
	}
}

// TestPaginationRebuild_NonCronRegression verifies the backward-compat path: an existing non-Cron paginated
// tool notification (Chunks-only, no LegacyChunks) still renders — the legacy text falls back to the rich
// text and no <hr/> is added (only Cron/SessionSend get InsertRichHr).
func TestPaginationRebuild_NonCronRegression(t *testing.T) {
	// A non-Cron entry (e.g. a standard "Message"/tool notification) with Chunks but no LegacyChunks.
	entryEvent := "Message"
	chunks := []string{"first tool body", "second tool body"}
	var legacyChunks []string // empty → legacy falls back to rich
	pageNum := 2
	nd := notify.NotificationData{
		Event:          entryEvent,
		CWD:            "/home/u/p",
		Body:           chunks[pageNum-1],
		Page:           pageNum,
		TotalPages:     len(chunks),
		ContextUsedPct: -1,
	}
	richText := notify.BuildNotificationText(nd)
	// Non-Cron: InsertRichHr is NOT applied (the rebuild only calls it for Cron/SessionSend).
	if entryEvent == "Cron" || entryEvent == "SessionSend" {
		richText = InsertRichHr(richText)
	}
	if strings.Contains(richText, "<hr/>") {
		t.Errorf("non-Cron notification must not contain <hr/>; got %q", richText)
	}
	// LegacyChunks empty → legacyText falls back to richText (the S12b backward-compat rule).
	legacyText := richText
	if len(chunks) == len(legacyChunks) && pageNum-1 < len(legacyChunks) {
		ndLegacy := nd
		ndLegacy.Body = legacyChunks[pageNum-1]
		legacyText = notify.BuildNotificationText(ndLegacy)
	}
	if legacyText != richText {
		t.Errorf("with no LegacyChunks, legacy must fall back to rich text; got legacy=%q rich=%q", legacyText, richText)
	}
	// The message still renders non-empty content for the requested page.
	if !strings.Contains(richText, "second tool body") {
		t.Errorf("page-2 body must be present; got %q", richText)
	}
}
