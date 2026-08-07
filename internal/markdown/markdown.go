package markdown

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
)

var slashCmdRe = regexp.MustCompile(`^/\w+$`)

// SlashCommands is the set of known CC slash commands (without leading /).
// Set by cmd package at startup. If nil, no slash command detection is performed.
var SlashCommands map[string]bool

// EscapeHTML escapes <, >, & for Telegram HTML parse mode.
func EscapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// TableData represents a parsed markdown table.
type TableData struct {
	Headers     []string
	Rows        [][]string
	HeadersHTML []string
	RowsHTML    [][]string
}

// ContainsTables returns true if md contains GFM tables.
func ContainsTables(md string) bool {
	md = normalizeTableBold(md)
	gm := goldmark.New(goldmark.WithExtensions(extension.GFM))
	reader := text.NewReader([]byte(md))
	doc := gm.Parser().Parse(reader)
	found := false
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if _, ok := n.(*extast.Table); ok {
			found = true
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return found
}

// ExtractTableData parses md and returns all tables as structured data.
func ExtractTableData(md string) []TableData {
	md = normalizeTableBold(md)
	source := []byte(md)
	gm := goldmark.New(goldmark.WithExtensions(extension.GFM))
	reader := text.NewReader(source)
	doc := gm.Parser().Parse(reader)
	var tables []TableData
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		table, ok := n.(*extast.Table)
		if !ok {
			return ast.WalkContinue, nil
		}
		var td TableData
		for row := table.FirstChild(); row != nil; row = row.NextSibling() {
			var cells []string
			var cellsHTML []string
			for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
				cells = append(cells, collectCellText(cell, source))
				cellsHTML = append(cellsHTML, collectCellHTML(cell, source))
			}
			if _, isHeader := row.(*extast.TableHeader); isHeader {
				td.Headers = cells
				td.HeadersHTML = cellsHTML
			} else {
				td.Rows = append(td.Rows, cells)
				td.RowsHTML = append(td.RowsHTML, cellsHTML)
			}
		}
		tables = append(tables, td)
		return ast.WalkSkipChildren, nil
	})
	return tables
}

// RemoveTables removes all GFM table blocks from markdown source, returning the remaining text.
// Uses line-based matching for robustness — any consecutive block of lines starting with |
// (outside code blocks) is replaced with a 📊 [Table] placeholder.
func RemoveTables(md string) string {
	md = normalizeTableBold(md)
	if !ContainsTables(md) {
		return md
	}
	lines := strings.Split(md, "\n")
	var result []string
	inTable := false
	inCodeBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
		}
		if inCodeBlock {
			result = append(result, line)
			continue
		}
		isTableLine := strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2
		if isTableLine {
			if !inTable {
				inTable = true
				result = append(result, "📊 [Table]")
			}
		} else {
			inTable = false
			result = append(result, line)
		}
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

// normalizeTableBold adds a space between ** and | in table rows so goldmark
// doesn't misparse **| as bold-end followed by non-separator pipe.
func normalizeTableBold(md string) string {
	lines := strings.Split(md, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") {
			lines[i] = strings.ReplaceAll(line, "**|", "** |")
		}
	}
	return strings.Join(lines, "\n")
}

// tgRenderer is a custom goldmark renderer that outputs Telegram-compatible HTML.
type tgRenderer struct {
	ordinal       map[ast.Node]int // tracks ordered list item counters
	skipCodeNodes map[ast.Node]bool
	rich          bool // when true, emit Bot API 10.1 rich HTML dialect
	inCell        bool // when true, we are inside a <td>/<th> (cells inline-only, C4)
}

// AddOptions implements renderer.Renderer (no-op for options).
func (r *tgRenderer) AddOptions(...renderer.Option) {}

// isBlockElement returns true if the node is a block-level element (code block, blockquote, table, list, heading).
func isBlockElement(n ast.Node) bool {
	if n == nil {
		return false
	}
	switch n.(type) {
	case *ast.FencedCodeBlock, *ast.CodeBlock, *ast.Blockquote, *ast.Heading, *ast.List:
		return true
	case *extast.Table:
		return true
	}
	return false
}

// listDepth returns the nesting depth of a ListItem node (0 for top-level).
func listDepth(n ast.Node) int {
	depth := 0
	for p := n.Parent(); p != nil; p = p.Parent() {
		if _, ok := p.(*ast.List); ok {
			depth++
		}
	}
	// Subtract 1 because the immediate parent List counts as depth 0
	if depth > 0 {
		depth--
	}
	return depth
}

// Render walks the AST and writes Telegram HTML to w.
func (r *tgRenderer) Render(w io.Writer, source []byte, node ast.Node) error {
	r.ordinal = make(map[ast.Node]int)
	r.skipCodeNodes = make(map[ast.Node]bool)
	return ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		return r.renderNode(w, source, n, entering)
	})
}

func (r *tgRenderer) renderNode(w io.Writer, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch node := n.(type) {
	case *ast.Document:
		// Nothing to do

	case *ast.Heading:
		if r.rich {
			// Rich headings render at their original level (h1→h1 … h6→h6); no downgrade/cap
			// (boss decision; Bot API RichBlockSectionHeading supports h1-h6).
			tag := fmt.Sprintf("h%d", node.Level)
			if entering {
				fmt.Fprintf(w, "<%s>", tag)
			} else {
				// D2: <h*> is a rich block element (self-breaks) — no trailing "\n\n" (which rich
				// HTML would collapse; a centralized RichifyNewlines would otherwise make it <br><br>).
				fmt.Fprintf(w, "</%s>", tag)
			}
		} else {
			if entering {
				fmt.Fprintf(w, "<b>")
			} else {
				fmt.Fprintf(w, "</b>\n\n")
			}
		}

	case *ast.Paragraph:
		if r.inCell {
			// C4: table-cell content is inline-only — no <p>, no newline separators.
			break
		}
		if r.rich {
			_, inListItem := n.Parent().(*ast.ListItem)
			_, inBlockquote := n.Parent().(*ast.Blockquote)
			if inListItem || inBlockquote {
				// Inside <li>/<blockquote>: content is inline (no <p> wrap). Separate a following
				// sibling content block with <br> so consecutive blocks do not merge to one line.
				if !entering {
					if next := n.NextSibling(); next != nil {
						if _, isList := next.(*ast.List); !isList {
							fmt.Fprintf(w, "<br>")
						}
					}
				}
			} else if n.PreviousSibling() != nil || n.NextSibling() != nil {
				// Multiple top-level blocks → wrap this paragraph in a real <p> so consecutive
				// paragraphs/blocks don't merge onto one line. A SOLE paragraph is left unwrapped so
				// it stays inline (its internal soft breaks already emit <br>) — this keeps
				// single-line renders (e.g. a mailbox "Subject:" value) on the same line as their label.
				if entering {
					fmt.Fprintf(w, "<p>")
				} else {
					fmt.Fprintf(w, "</p>")
				}
			}
		} else if !entering {
			if _, ok := n.Parent().(*ast.ListItem); ok {
				// Inside list item — only add newline between sibling content blocks
				if next := n.NextSibling(); next != nil {
					if _, isList := next.(*ast.List); !isList {
						fmt.Fprintf(w, "\n")
					}
				}
			} else if _, ok := n.Parent().(*ast.Blockquote); ok {
				fmt.Fprintf(w, "\n")
			} else if next := n.NextSibling(); isBlockElement(next) {
				fmt.Fprintf(w, "\n\n")
			} else {
				fmt.Fprintf(w, "\n\n")
			}
		}

	case *ast.Blockquote:
		if r.inCell {
			// C4: blockquote inside a cell — suppress tags, just emit text content
		} else {
			if entering {
				fmt.Fprintf(w, "<blockquote>")
			} else {
				fmt.Fprintf(w, "</blockquote>")
			}
		}

	case *ast.FencedCodeBlock:
		if entering {
			var buf bytes.Buffer
			lines := node.Lines()
			for i := 0; i < lines.Len(); i++ {
				line := lines.At(i)
				buf.Write(line.Value(source))
			}
			content := ExpandTabs(EscapeHTML(strings.TrimRight(buf.String(), "\n")))
			if r.rich && r.inCell {
				// C4: inside a table cell — flatten fenced code to inline <code>
				fmt.Fprintf(w, "<code>%s</code>", content)
			} else if r.rich {
				lang := ""
				if node.Info != nil {
					lang = strings.TrimSpace(string(node.Info.Segment.Value(source)))
				}
				if lang != "" {
					// D2: <pre> self-breaks; no trailing "\n\n" outside the block. Newlines INSIDE
					// <pre> (the code) are preserved by RichifyNewlines.
					fmt.Fprintf(w, "<pre><code class=\"language-%s\">%s</code></pre>", EscapeHTML(lang), content)
				} else {
					fmt.Fprintf(w, "<pre><code>%s</code></pre>", content)
				}
			} else {
				fmt.Fprintf(w, "<pre><code>%s</code></pre>\n\n", content)
			}
		}
		return ast.WalkSkipChildren, nil

	case *ast.CodeBlock:
		if entering {
			var buf bytes.Buffer
			lines := node.Lines()
			for i := 0; i < lines.Len(); i++ {
				line := lines.At(i)
				buf.Write(line.Value(source))
			}
			content := ExpandTabs(EscapeHTML(strings.TrimRight(buf.String(), "\n")))
			if r.inCell {
				// C4: flatten to inline <code>
				fmt.Fprintf(w, "<code>%s</code>", content)
			} else if r.rich {
				// D2: <pre> self-breaks; no trailing "\n\n" in rich.
				fmt.Fprintf(w, "<pre><code>%s</code></pre>", content)
			} else {
				fmt.Fprintf(w, "<pre><code>%s</code></pre>\n\n", content)
			}
		}
		return ast.WalkSkipChildren, nil

	case *ast.CodeSpan:
		if entering {
			var cmdBuf bytes.Buffer
			for child := node.FirstChild(); child != nil; child = child.NextSibling() {
				if t, ok := child.(*ast.Text); ok {
					cmdBuf.Write(t.Segment.Value(source))
				}
			}
			content := strings.TrimSpace(cmdBuf.String())
			if SlashCommands != nil && slashCmdRe.MatchString(content) && SlashCommands[content[1:]] {
				fmt.Fprintf(w, "%s", EscapeHTML(content))
				r.skipCodeNodes[n] = true
				return ast.WalkSkipChildren, nil
			}
			fmt.Fprintf(w, "<code>")
		} else {
			if !r.skipCodeNodes[n] {
				fmt.Fprintf(w, "</code>")
			}
		}

	case *ast.Emphasis:
		if node.Level == 2 {
			if entering {
				fmt.Fprintf(w, "<b>")
			} else {
				fmt.Fprintf(w, "</b>")
			}
		} else {
			if entering {
				fmt.Fprintf(w, "<i>")
			} else {
				fmt.Fprintf(w, "</i>")
			}
		}

	case *extast.Strikethrough:
		if entering {
			fmt.Fprintf(w, "<s>")
		} else {
			fmt.Fprintf(w, "</s>")
		}

	case *ast.Link:
		if entering {
			fmt.Fprintf(w, `<a href="%s">`, EscapeHTML(string(node.Destination)))
		} else {
			fmt.Fprintf(w, "</a>")
		}

	case *ast.AutoLink:
		if entering {
			url := string(node.URL(source))
			fmt.Fprintf(w, `<a href="%s">%s</a>`, EscapeHTML(url), EscapeHTML(url))
		}
		return ast.WalkSkipChildren, nil

	case *ast.List:
		if r.rich && r.inCell {
			// C4: inside a table cell — flatten list items to inline text separated by " • "
			// We skip the <ul>/<ol> wrapper and handle items inline in *ast.ListItem
		} else if r.rich {
			if entering {
				if node.IsOrdered() {
					fmt.Fprintf(w, "<ol>")
				} else {
					fmt.Fprintf(w, "<ul>")
				}
			} else {
				// D2: <ul>/<ol> self-break; no trailing "\n\n" in rich.
				if node.IsOrdered() {
					fmt.Fprintf(w, "</ol>")
				} else {
					fmt.Fprintf(w, "</ul>")
				}
			}
		} else {
			if entering {
				// Add newline before nested list (after parent item text)
				if _, ok := n.Parent().(*ast.ListItem); ok {
					fmt.Fprintf(w, "\n")
				}
			} else {
				// Only add spacing after top-level lists, not nested ones
				if _, ok := n.Parent().(*ast.ListItem); !ok {
					fmt.Fprintf(w, "\n")
				}
			}
		}

	case *ast.ListItem:
		if r.rich && r.inCell {
			// C4: flatten list items to inline text; add bullet separator between items
			if entering {
				// Add separator before non-first items
				if node.PreviousSibling() != nil {
					fmt.Fprintf(w, " • ")
				}
			}
		} else if r.rich {
			if entering {
				fmt.Fprintf(w, "<li>")
			} else {
				fmt.Fprintf(w, "</li>")
			}
		} else {
			if entering {
				depth := listDepth(node)
				indent := strings.Repeat("  ", depth)
				parent, ok := node.Parent().(*ast.List)
				if ok && parent.IsOrdered() {
					r.ordinal[parent]++
					fmt.Fprintf(w, "%s%d. ", indent, r.ordinal[parent])
				} else {
					fmt.Fprintf(w, "%s• ", indent)
				}
			} else {
				// Don't add newline if last child is a nested list (already ends with newline)
				if lastChild := node.LastChild(); lastChild != nil {
					if _, isList := lastChild.(*ast.List); isList {
						return ast.WalkContinue, nil
					}
				}
				fmt.Fprintf(w, "\n")
			}
		}

	case *extast.Table:
		if r.rich {
			if entering {
				// Bot API 10.1 <table bordered striped>: borders + zebra row striping (rich-html-style).
				fmt.Fprintf(w, "<table bordered striped>")
			} else {
				// D2: <table> self-breaks; no trailing "\n\n" in rich.
				fmt.Fprintf(w, "</table>")
			}
		} else {
			if entering {
				var buf bytes.Buffer
				renderTableFallback(&buf, node, source)
				if buf.Len() > 0 {
					fmt.Fprintf(w, "<pre>%s</pre>\n\n", ExpandTabs(EscapeHTML(buf.String())))
				}
			}
			return ast.WalkSkipChildren, nil
		}

	case *extast.TableHeader, *extast.TableRow:
		if !r.rich {
			// Handled by Table above (WalkSkipChildren) in legacy mode
			break
		}
		if entering {
			fmt.Fprintf(w, "<tr>")
		} else {
			fmt.Fprintf(w, "</tr>")
		}

	case *extast.TableCell:
		if !r.rich {
			// Handled by Table above in legacy mode
			break
		}
		if entering {
			// Render cell inline-only (C4): collect inline HTML from cell children
			html := collectCellHTMLRich(node, source)
			fmt.Fprintf(w, "<td>%s</td>", html)
		}
		return ast.WalkSkipChildren, nil

	case *ast.ThematicBreak:
		if entering {
			if r.rich {
				// D2: sits between self-breaking blocks; a trailing "\n\n" would collapse. Wrap in
				// <br> so the rule is on its own line even between non-block neighbors.
				fmt.Fprintf(w, "<br>───────<br>")
			} else {
				fmt.Fprintf(w, "───────\n\n")
			}
		}

	case *ast.Text:
		if entering {
			seg := node.Segment
			txt := EscapeHTML(string(seg.Value(source)))
			fmt.Fprintf(w, "%s", txt)
			if node.SoftLineBreak() || node.HardLineBreak() {
				if r.rich {
					// Rich HTML collapses a bare "\n" to a space; emit an explicit line break.
					fmt.Fprintf(w, "<br>")
				} else {
					fmt.Fprintf(w, "\n")
				}
			}
		}

	case *ast.String:
		if entering {
			fmt.Fprintf(w, "%s", EscapeHTML(string(node.Value)))
		}

	case *ast.RawHTML:
		// Skip raw HTML — Telegram HTML only supports specific tags
		return ast.WalkSkipChildren, nil

	case *ast.HTMLBlock:
		return ast.WalkSkipChildren, nil

	case *ast.Image:
		// Render alt text only
		if entering {
			fmt.Fprintf(w, "[image: ")
		} else {
			fmt.Fprintf(w, "]")
		}
	}
	return ast.WalkContinue, nil
}

// ExpandTabs expands leading tabs to 2 spaces on each line.
func ExpandTabs(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		j := 0
		spaceCount := 0
		for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
			if line[j] == '\t' {
				spaceCount += 2
			} else {
				spaceCount++
			}
			j++
		}
		if j > 0 {
			lines[i] = strings.Repeat(" ", spaceCount) + line[j:]
		}
	}
	return strings.Join(lines, "\n")
}

// renderTableFallback renders table rows directly as markdown-style pipe table text.
func renderTableFallback(buf *bytes.Buffer, table *extast.Table, source []byte) {
	// Collect all rows
	var allRows [][]string
	headerIdx := -1
	for row := table.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, collectCellText(cell, source))
		}
		if _, ok := row.(*extast.TableHeader); ok {
			headerIdx = len(allRows)
		}
		allRows = append(allRows, cells)
	}
	if len(allRows) == 0 {
		return
	}
	// Calculate max column widths using displayWidth
	numCols := 0
	for _, row := range allRows {
		if len(row) > numCols {
			numCols = len(row)
		}
	}
	colWidths := make([]int, numCols)
	for _, row := range allRows {
		for i, cell := range row {
			w := displayWidth(cell)
			if w > colWidths[i] {
				colWidths[i] = w
			}
		}
	}
	// Output aligned table
	for i, row := range allRows {
		buf.WriteString("|")
		for j := 0; j < numCols; j++ {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			pad := colWidths[j] - displayWidth(cell)
			buf.WriteString(" ")
			buf.WriteString(cell)
			for k := 0; k < pad; k++ {
				buf.WriteByte(' ')
			}
			buf.WriteString(" |")
		}
		buf.WriteByte('\n')
		if i == headerIdx {
			buf.WriteString("|")
			for j := 0; j < numCols; j++ {
				buf.WriteString("-")
				for k := 0; k < colWidths[j]; k++ {
					buf.WriteByte('-')
				}
				buf.WriteString("-|")
			}
			buf.WriteByte('\n')
		}
	}
}

// collectCellText extracts all text from a table cell node.
func collectCellText(n ast.Node, source []byte) string {
	var buf bytes.Buffer
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		ast.Walk(child, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			if t, ok := c.(*ast.Text); ok {
				buf.Write(t.Segment.Value(source))
			} else if s, ok := c.(*ast.String); ok {
				buf.Write(s.Value)
			}
			return ast.WalkContinue, nil
		})
	}
	return strings.TrimSpace(buf.String())
}

// collectCellHTML extracts cell content as HTML, preserving bold/italic formatting.
func collectCellHTML(n ast.Node, source []byte) string {
	var buf bytes.Buffer
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		ast.Walk(child, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
			switch v := c.(type) {
			case *ast.Text:
				if entering {
					buf.WriteString(htmlEscape(string(v.Segment.Value(source))))
				}
			case *ast.String:
				if entering {
					buf.WriteString(htmlEscape(string(v.Value)))
				}
			case *ast.Emphasis:
				if v.Level == 2 {
					if entering {
						buf.WriteString("<strong>")
					} else {
						buf.WriteString("</strong>")
					}
				} else {
					if entering {
						buf.WriteString("<em>")
					} else {
						buf.WriteString("</em>")
					}
				}
			}
			return ast.WalkContinue, nil
		})
	}
	return strings.TrimSpace(buf.String())
}

// htmlEscape escapes HTML special chars for table cell content.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// collectCellHTMLRich collects cell content as rich inline HTML (C4: cells inline-only).
// Block-level children (fenced code → <code>, lists → bullet-separated text, blockquote → text)
// are flattened to inline equivalents. Only inline rich tags are emitted.
func collectCellHTMLRich(n ast.Node, source []byte) string {
	var buf bytes.Buffer
	r := &tgRenderer{rich: true, inCell: true, ordinal: make(map[ast.Node]int), skipCodeNodes: make(map[ast.Node]bool)}
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		ast.Walk(child, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
			return r.renderNode(&buf, source, c, entering)
		})
	}
	return strings.TrimSpace(buf.String())
}

// EscapeRich escapes a string for embedding in rich HTML, restricting to the allowed named-entity
// set (only &lt; &gt; &amp; &quot; &apos; &nbsp; &hellip; &mdash; &ndash; &lsquo; &rsquo; &ldquo;
// &rdquo; are allowed; all other named entities are escaped to numeric form).
func EscapeRich(s string) string {
	// First escape the base HTML characters so they become allowed named entities
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

// RenderTelegramHTML converts Markdown text to Telegram-compatible HTML.
func RenderTelegramHTML(md string) string {
	md = normalizeTableBold(md)
	gm := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
	)
	var buf bytes.Buffer
	reader := text.NewReader([]byte(md))
	doc := gm.Parser().Parse(reader)
	r := &tgRenderer{}
	if err := r.Render(&buf, []byte(md), doc); err != nil {
		// Fallback: escape raw text
		return EscapeHTML(md)
	}
	result := buf.String()
	// Collapse 3+ consecutive newlines to 2 (one blank line max)
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(result, "\n")
}

// RenderRichHTML converts Markdown text to Bot API 10.1 rich HTML dialect.
// Uses real block tags: <h1>..<h6>, <ul>/<ol>/<li>, <table><tr><td>,
// <pre><code class="language-x">. Table cells are inline-only (C4).
// Leave RenderTelegramHTML unchanged — it is the legacy/fallback path.
func RenderRichHTML(md string) string {
	md = normalizeTableBold(md)
	gm := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
	)
	var buf bytes.Buffer
	reader := text.NewReader([]byte(md))
	doc := gm.Parser().Parse(reader)
	r := &tgRenderer{rich: true}
	if err := r.Render(&buf, []byte(md), doc); err != nil {
		return EscapeRich(md)
	}
	// D2: rich block elements no longer emit trailing "\n\n"; the only remaining newlines live inside
	// <pre> (code), so DON'T run the legacy "\n\n\n"->"\n\n" squeeze here — it would collapse blank
	// lines inside code blocks. RichifyNewlines (applied at send time) handles the <br> conversion.
	result := buf.String()
	// Bold fallback: goldmark leaves **bold** unparsed when the closing ** is not right-flanking —
	// e.g. a punctuation char (fullwidth "）" or ASCII ")") immediately before ** followed by a CJK
	// letter ("**A（注）**已" stays literal). Convert those survivors to <b> here.
	result = applyBoldFallback(result)
	return strings.TrimRight(result, "\n")
}

// boldFallbackPreserveRe matches regions where a literal "**" must NOT be treated as a bold
// delimiter: <pre> (code/terminal), inline <code>, and <tg-math-block> (math source). <pre> comes
// first so a code block (with its inner </code>) is consumed whole. Non-greedy per region.
var boldFallbackPreserveRe = regexp.MustCompile(`(?s)<pre>.*?</pre>|<code>.*?</code>|<tg-math-block>.*?</tg-math-block>`)

// boldFallbackRe matches a "**...**" run goldmark left unparsed. Content is non-empty, single-line,
// and free of other "*" so overlapping/partial runs are not merged.
var boldFallbackRe = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)

// applyBoldFallback converts surviving "**...**" pairs to <b>...</b>, OUTSIDE preserved regions
// (<pre>/<code>/<tg-math-block>). Operates on already-escaped rich HTML, so injecting <b>/</b> is
// safe. No-op when the html carries no "**".
func applyBoldFallback(html string) string {
	if !strings.Contains(html, "**") {
		return html
	}
	var b strings.Builder
	last := 0
	for _, loc := range boldFallbackPreserveRe.FindAllStringIndex(html, -1) {
		b.WriteString(boldFallbackRe.ReplaceAllString(html[last:loc[0]], "<b>$1</b>"))
		b.WriteString(html[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(boldFallbackRe.ReplaceAllString(html[last:], "<b>$1</b>"))
	return b.String()
}

// richPreserveRe matches regions whose literal newlines are significant in the rich HTML dialect
// and MUST NOT be converted to <br>: <pre> blocks (whitespace-preserving code/terminal) and
// <tg-math-block> (math source). Non-greedy so adjacent regions are matched separately.
var richPreserveRe = regexp.MustCompile(`(?s)<pre>.*?</pre>|<tg-math-block>.*?</tg-math-block>`)

// RichifyNewlines converts literal newlines to <br> for the Bot API 10.1 rich_message.html dialect,
// where html has REAL HTML whitespace semantics — a bare "\n" collapses to a single space (unlike
// legacy parse_mode=HTML, where "\n" is a line break). Newlines INSIDE <pre> and <tg-math-block>
// regions are preserved verbatim. Idempotent: a second pass finds no convertible "\n" (all
// remaining newlines live inside preserved regions), so re-feeding already-converted html is a
// no-op. Apply ONLY to the rich html payload, never to the legacy HTML fallback (G3).
func RichifyNewlines(html string) string {
	if strings.Contains(html, "\n") {
		var b strings.Builder
		last := 0
		for _, loc := range richPreserveRe.FindAllStringIndex(html, -1) {
			b.WriteString(strings.ReplaceAll(html[last:loc[0]], "\n", "<br>"))
			b.WriteString(html[loc[0]:loc[1]])
			last = loc[1]
		}
		b.WriteString(strings.ReplaceAll(html[last:], "\n", "<br>"))
		html = b.String()
	}
	return stripBrAroundDetails(html)
}

// stripBrAroundDetails removes the <br> that is redundant next to a block-level boundary: a <br>
// immediately BEFORE an opening <details, immediately AFTER a closing </details>, immediately AFTER a
// </summary>, or immediately AFTER an <hr/> (a self-breaking rule needs no following line break — the
// mailbox separator relies on this so the body sits directly under the rule). A <br> immediately BEFORE
// a closing </details> is KEPT. <details>/<summary> tags inside <pre>/<tg-math-block> regions are
// HTML-escaped (&lt;details&gt;), so these real-tag replacements never touch them. Idempotent (a second
// pass finds no such adjacency). The replacements are order-independent: a <br> between adjacent details
// (</details><br><details>) collapses either way.
func stripBrAroundDetails(html string) string {
	if !strings.Contains(html, "<br>") {
		return html
	}
	html = strings.ReplaceAll(html, "<br><details", "<details")
	html = strings.ReplaceAll(html, "</details><br>", "</details>")
	html = strings.ReplaceAll(html, "</summary><br>", "</summary>")
	html = strings.ReplaceAll(html, "<hr/><br>", "<hr/>")
	return html
}

// notificationLeadEmojis are the leading emojis of buildHeader-based notifications (BuildNotificationText
// statusLine, BuildPermissionText, BuildQuestionText). CollapseSessionMeta only collapses a message whose
// FIRST line starts with one of these — this excludes any message that happens to carry a "📂 " line,
// preventing false positives. Cron (🔔 / 📨) and SessionSend (💬 / 📨) now flow through BuildNotificationText
// with a neutral header, so their lead emojis are included here to collapse their metadata like other
// notifications. Adding a new notification event with a new leading emoji requires adding it here
// (intentionally default-off for unknown leading tokens).
// f29 G: the BARE pen (U+1F58A, no VS16) is added so a SessionSend visible line led by the VS16-suffixed
// pen (🖊️ = U+1F58A U+FE0F) still matches via strings.HasPrefix (the bare rune is a prefix of both forms).
var notificationLeadEmojis = []string{"🟢", "🔴", "💬", "🔧", "✅", "🔐", "❓", "🔔", "📨", "\U0001F58A"}

// locationLeads are the metadata lines buildHeader emits at line 1+ (order-preserving), before the
// optional 📊 Context line. They are the run-CONTINUATION set; the collapse ANCHOR (line 1) is NARROWER
// — see CollapseSessionMetaWithID.
var locationLeads = []string{"📂 ", "👤 ", "🏷 ", "📟 ", "🖥 "}

func isLocationLine(s string) bool {
	for _, e := range locationLeads {
		if strings.HasPrefix(s, e) {
			return true
		}
	}
	return false
}

// CollapseSessionMeta wraps the header metadata block (👤 sender, 📂 CWD, 📟 tmux, 🖥 CLI command, 📊
// Context) of a rich notification in a default-collapsed <details> block, to save vertical space.
// Rich-path only: it is applied in the rich sender wrappers and never to the legacy fallback. It anchors
// to HEADER POSITION — the block is wrapped ONLY when line 0 starts with a notification lead emoji AND
// line 1 starts with "📂 " OR "👤 " (f29: a folder or sender line — the two lines buildHeader emits first;
// a 📟 pane/🖥 CLI line at index 1 is NOT an anchor so a non-header notification like
// "✅ Injected …\n📟 …" is never collapsed). The <summary> is a COMPACT context status
// (📋 C:<pct> (<used>/<total>)) extracted from the 📊 Context line — falling back to the fixed 📋 Session
// label when no context line is present. ALL metadata (the 👤/📂/📟/🖥 location lines first, then the full
// 📊 Context line) lives INSIDE the block; only the body stays OUTSIDE. No-op for anything else
// (stream/tool bodies, <pre> captures, pane-lead notifications), so a location-emoji lookalike deeper in
// the body is never matched.
func CollapseSessionMeta(text string) string {
	return CollapseSessionMetaWithID(text, 0)
}

// CollapseSessionMetaWithID is CollapseSessionMeta with an optional tg-cli message ID. When msgID > 0 a
// "🆔 #<msgID>" line is placed first inside the <details> block (before the location + context lines);
// msgID == 0 omits it (the plain CollapseSessionMeta behaviour used by the logger call sites).
func CollapseSessionMetaWithID(text string, msgID int64) string {
	lines := strings.Split(text, "\n")
	// ANCHOR (f29): the metadata run must START (line 1) with a folder (📂) or sender (👤) line — NOT a
	// pane/CLI line. buildHeader always emits 📂 or 👤 first for a real notification header; restricting
	// the anchor keeps a non-header notification that merely starts with a 📟 pane line (e.g.
	// cmd/bot_helpers.go:544 "✅ Injected …\n📟 …") from being collapsed.
	if len(lines) < 2 || (!strings.HasPrefix(lines[1], "📂 ") && !strings.HasPrefix(lines[1], "👤 ")) {
		return text
	}
	lead := false
	for _, e := range notificationLeadEmojis {
		if strings.HasPrefix(lines[0], e) {
			lead = true
			break
		}
	}
	if !lead {
		return text
	}
	// Contiguous metadata run from line 1: the location lines (📂/👤/📟/🖥 in buildHeader emission order),
	// then an optional 📊 Context line.
	metaEnd := 1
	for metaEnd < len(lines) && isLocationLine(lines[metaEnd]) {
		metaEnd++
	}
	locationLines := lines[1:metaEnd] // 📂/👤/📟/🖥 in emission order
	end := metaEnd
	contextLine := ""
	if end < len(lines) && strings.HasPrefix(lines[end], "📊 Context: ") {
		contextLine = lines[end]
		end++
	}
	// Summary = compact context status (📋 C:<pct> (<used>/<total>)) when the 📊 Context line is present,
	// else the fixed 📋 Session label. The detail block holds the 📂/📟/🖥 location lines first, then the
	// full 📊 Context line last (if any), matching the original unfolded header order. Only the body
	// (lines[end:]) stays outside.
	summary := "📋 Session"
	detailLines := make([]string, 0, len(locationLines)+2)
	if contextLine != "" {
		summary = "📋 C:" + strings.TrimPrefix(contextLine, "📊 Context: ")
	}
	// 🆔 message ID first (Feature 2), then the 📂/📟/🖥 location lines, then the 📊 Context line.
	if msgID > 0 {
		detailLines = append(detailLines, "🆔 #"+strconv.FormatInt(msgID, 10))
	}
	detailLines = append(detailLines, locationLines...)
	if contextLine != "" {
		detailLines = append(detailLines, contextLine)
	}
	block := "<details><summary>" + summary + "</summary>\n" + strings.Join(detailLines, "\n") + "\n</details>"
	body := lines[end:]
	// The collapsed <details> header replaces the old unfolded header, so the blank line that used to
	// separate the header from the body is now dead vertical space — drop a single leading blank line.
	if len(body) > 0 && body[0] == "" {
		body = body[1:]
	}
	out := append([]string{lines[0], block}, body...)
	return strings.Join(out, "\n")
}
