package markdown

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
)

// RenderTableImage renders a table as a PNG image.
func RenderTableImage(headers []string, rows [][]string) ([]byte, error) {
	const (
		cellPadX  = 42
		cellPadY  = 30
		fontSize  = 42.0
		lineWidth = 3.0
	)

	face, err := loadFont(fontSize)
	if err != nil {
		return nil, err
	}
	emojiFace := loadEmojiFont(fontSize)

	// Measure column widths
	numCols := len(headers)
	colWidths := make([]float64, numCols)
	dc := gg.NewContext(1, 1)
	dc.SetFontFace(face)
	for i, h := range headers {
		w := measureStringWithFallback(dc, h, face, emojiFace)
		if w > colWidths[i] {
			colWidths[i] = w
		}
	}
	for _, row := range rows {
		for i := 0; i < numCols && i < len(row); i++ {
			w := measureStringWithFallback(dc, row[i], face, emojiFace)
			if w > colWidths[i] {
				colWidths[i] = w
			}
		}
	}
	// Add padding
	for i := range colWidths {
		colWidths[i] += 2 * cellPadX
	}

	// Calculate dimensions
	rowHeight := fontSize + 2*cellPadY
	totalWidth := lineWidth
	for _, w := range colWidths {
		totalWidth += w + lineWidth
	}
	totalHeight := lineWidth + rowHeight*float64(1+len(rows)) + lineWidth*float64(len(rows))

	// Create context
	dc = gg.NewContext(int(totalWidth)+1, int(totalHeight)+1)
	dc.SetFontFace(face)

	// Fill white background
	dc.SetColor(color.White)
	dc.Clear()

	// Draw header background
	headerBg := color.RGBA{R: 0xE8, G: 0xE8, B: 0xE8, A: 0xFF}
	x := lineWidth
	for _, w := range colWidths {
		dc.SetColor(headerBg)
		dc.DrawRectangle(x, lineWidth, w, rowHeight)
		dc.Fill()
		x += w + lineWidth
	}

	// Draw text
	dc.SetColor(color.Black)
	y := lineWidth + cellPadY + fontSize
	x = lineWidth
	for i, h := range headers {
		drawStringWithFallback(dc, h, x+cellPadX, y, face, emojiFace)
		x += colWidths[i] + lineWidth
	}
	for _, row := range rows {
		y += rowHeight + lineWidth
		x = lineWidth
		for i := 0; i < numCols; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			drawStringWithFallback(dc, cell, x+cellPadX, y, face, emojiFace)
			x += colWidths[i] + lineWidth
		}
	}

	// Draw grid lines
	gridColor := color.RGBA{R: 0xCC, G: 0xCC, B: 0xCC, A: 0xFF}
	dc.SetColor(gridColor)
	dc.SetLineWidth(lineWidth)
	// Horizontal lines
	for i := 0; i <= 1+len(rows); i++ {
		ly := lineWidth/2 + float64(i)*(rowHeight+lineWidth)
		dc.DrawLine(0, ly, totalWidth, ly)
		dc.Stroke()
	}
	// Vertical lines
	x = lineWidth / 2
	dc.DrawLine(x, 0, x, totalHeight)
	dc.Stroke()
	for _, w := range colWidths {
		x += w + lineWidth
		dc.DrawLine(x, 0, x, totalHeight)
		dc.Stroke()
	}

	// Encode PNG
	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RenderTableImageChrome renders a table as a PNG using headless Chrome (supports color emoji).
func RenderTableImageChrome(headers []string, rows [][]string) ([]byte, error) {
	html := buildTableHTML(headers, rows)
	tmpFile, err := os.CreateTemp("", "tg-cli-table-*.html")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := tmpFile.WriteString(html); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoSandbox,
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 15*time.Second)
	defer cancelTimeout()
	var clip map[string]float64
	var buf []byte
	if err := chromedp.Run(ctx,
		chromedp.Navigate("file://"+tmpPath),
		chromedp.WaitReady("table"),
		chromedp.Evaluate(`(() => {
			const r = document.querySelector('table').getBoundingClientRect();
			const padX = r.width * 0.15;
			const padY = r.height * 0.15;
			document.body.style.padding = padY + 'px ' + padX + 'px';
			const r2 = document.querySelector('table').getBoundingClientRect();
			return {width: r2.x + r2.width + padX, height: r2.y + r2.height + padY};
		})()`, &clip),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			buf, err = page.CaptureScreenshot().
				WithClip(&page.Viewport{
					X: 0, Y: 0,
					Width:  math.Ceil(clip["width"]),
					Height: math.Ceil(clip["height"]),
					Scale:  3,
				}).Do(ctx)
			return err
		}),
	); err != nil {
		return nil, fmt.Errorf("chromedp render failed: %w", err)
	}
	return buf, nil
}

func buildTableHTML(headers []string, rows [][]string) string {
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"><style>
body { margin: 0; padding: 0; background: white; }
table { border-collapse: collapse; font-family: -apple-system, "Segoe UI", sans-serif; font-size: 16px; }
th, td { border: 1px solid #ccc; padding: 8px 16px; text-align: left; }
th { background: #E8E8E8; font-weight: bold; }
</style></head><body><table>`)
	sb.WriteString("<thead><tr>")
	for _, h := range headers {
		sb.WriteString("<th>")
		sb.WriteString(template.HTMLEscapeString(h))
		sb.WriteString("</th>")
	}
	sb.WriteString("</tr></thead><tbody>")
	for _, row := range rows {
		sb.WriteString("<tr>")
		for _, cell := range row {
			sb.WriteString("<td>")
			sb.WriteString(template.HTMLEscapeString(cell))
			sb.WriteString("</td>")
		}
		sb.WriteString("</tr>")
	}
	sb.WriteString("</tbody></table></body></html>")
	return sb.String()
}

func loadFont(size float64) (font.Face, error) {
	fontPath := filepath.Join(config.GetConfigDir(), "fonts", "NotoSansSC.ttf")
	if _, err := os.Stat(fontPath); err == nil {
		face, err := gg.LoadFontFace(fontPath, size)
		if err == nil {
			return face, nil
		}
		logger.Info(fmt.Sprintf("CJK font load failed: %v", err))
	}
	// Try to download
	if err := downloadFont(fontPath); err == nil {
		face, err := gg.LoadFontFace(fontPath, size)
		if err == nil {
			return face, nil
		}
		logger.Info(fmt.Sprintf("CJK font load failed after download: %v", err))
	} else {
		logger.Info(fmt.Sprintf("CJK font download failed: %v", err))
	}
	// Fallback to Go built-in font (no CJK support)
	logger.Info("Using fallback font (no CJK support)")
	f, err := truetype.Parse(goregular.TTF)
	if err != nil {
		return nil, err
	}
	return truetype.NewFace(f, &truetype.Options{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	}), nil
}

func downloadFont(dest string) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	url := "https://github.com/google/fonts/raw/main/ofl/notosanssc/NotoSansSC%5Bwght%5D.ttf"
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: status %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func isEmoji(r rune) bool {
	return unicode.Is(unicode.So, r) ||
		(r >= 0x1F600 && r <= 0x1F64F) || // Emoticons
		(r >= 0x1F300 && r <= 0x1F5FF) || // Misc Symbols and Pictographs
		(r >= 0x1F680 && r <= 0x1F6FF) || // Transport and Map
		(r >= 0x1F1E0 && r <= 0x1F1FF) || // Flags
		(r >= 0x2600 && r <= 0x26FF) ||   // Misc symbols
		(r >= 0x2700 && r <= 0x27BF) ||   // Dingbats
		(r >= 0xFE00 && r <= 0xFE0F) ||   // Variation Selectors
		(r >= 0x1F900 && r <= 0x1F9FF) || // Supplemental Symbols
		(r >= 0x1FA00 && r <= 0x1FA6F) || // Chess Symbols
		(r >= 0x1FA70 && r <= 0x1FAFF) || // Symbols Extended-A
		(r >= 0x200D && r <= 0x200D) ||   // ZWJ
		(r >= 0x20E3 && r <= 0x20E3)      // Combining Enclosing Keycap
}

func loadEmojiFont(size float64) font.Face {
	fontPath := filepath.Join(config.GetConfigDir(), "fonts", "NotoEmoji.ttf")
	if _, err := os.Stat(fontPath); err == nil {
		face, err := gg.LoadFontFace(fontPath, size)
		if err == nil {
			return face
		}
		logger.Info(fmt.Sprintf("Emoji font load failed: %v", err))
	}
	if err := downloadEmojiFont(fontPath); err == nil {
		face, err := gg.LoadFontFace(fontPath, size)
		if err == nil {
			return face
		}
		logger.Info(fmt.Sprintf("Emoji font load failed after download: %v", err))
	} else {
		logger.Info(fmt.Sprintf("Emoji font download failed: %v", err))
	}
	return nil
}

func downloadEmojiFont(dest string) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	url := "https://github.com/google/fonts/raw/main/ofl/notoemoji/NotoEmoji%5Bwght%5D.ttf"
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: status %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

type textRun struct {
	text  string
	emoji bool
}

func splitRuns(s string) []textRun {
	var runs []textRun
	var current []rune
	currentEmoji := false
	for _, r := range s {
		e := isEmoji(r)
		if len(current) > 0 && e != currentEmoji {
			runs = append(runs, textRun{text: string(current), emoji: currentEmoji})
			current = current[:0]
		}
		current = append(current, r)
		currentEmoji = e
	}
	if len(current) > 0 {
		runs = append(runs, textRun{text: string(current), emoji: currentEmoji})
	}
	return runs
}

func measureStringWithFallback(dc *gg.Context, s string, mainFace, emojiFace font.Face) float64 {
	if emojiFace == nil {
		w, _ := dc.MeasureString(s)
		return w
	}
	total := 0.0
	for _, run := range splitRuns(s) {
		if run.emoji {
			dc.SetFontFace(emojiFace)
		} else {
			dc.SetFontFace(mainFace)
		}
		w, _ := dc.MeasureString(run.text)
		total += w
	}
	dc.SetFontFace(mainFace)
	return total
}

func drawStringWithFallback(dc *gg.Context, s string, x, y float64, mainFace, emojiFace font.Face) {
	if emojiFace == nil {
		dc.DrawString(s, x, y)
		return
	}
	for _, run := range splitRuns(s) {
		if run.emoji {
			dc.SetFontFace(emojiFace)
		} else {
			dc.SetFontFace(mainFace)
		}
		dc.DrawString(run.text, x, y)
		w, _ := dc.MeasureString(run.text)
		x += w
	}
	dc.SetFontFace(mainFace)
}

// runeWidth returns approximate display width (CJK = 2, others = 1).
func runeWidth(r rune) int {
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x20000 && r <= 0x2fffd) ||
		(r >= 0x30000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

// displayWidth returns the display width of a string.
func displayWidth(s string) int {
	w := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		w += runeWidth(r)
		s = s[size:]
	}
	return w
}
