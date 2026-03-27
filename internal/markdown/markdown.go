package markdown

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
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
	Headers []string
	Rows    [][]string
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
			for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
				cells = append(cells, collectCellText(cell, source))
			}
			if _, isHeader := row.(*extast.TableHeader); isHeader {
				td.Headers = cells
			} else {
				td.Rows = append(td.Rows, cells)
			}
		}
		tables = append(tables, td)
		return ast.WalkSkipChildren, nil
	})
	return tables
}

// RemoveTables removes all GFM table blocks from markdown source, returning the remaining text.
func RemoveTables(md string) string {
	md = normalizeTableBold(md)
	source := []byte(md)
	gm := goldmark.New(goldmark.WithExtensions(extension.GFM))
	reader := text.NewReader(source)
	doc := gm.Parser().Parse(reader)
	// Collect byte ranges of table nodes
	type byteRange struct{ start, end int }
	var ranges []byteRange
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if _, ok := n.(*extast.Table); ok {
			// Find start and end byte positions from table's line segments
			start := -1
			end := -1
			for row := n.FirstChild(); row != nil; row = row.NextSibling() {
				for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
					for child := cell.FirstChild(); child != nil; child = child.NextSibling() {
						if t, ok := child.(*ast.Text); ok {
							seg := t.Segment
							if start == -1 || seg.Start < start {
								start = seg.Start
							}
							if seg.Stop > end {
								end = seg.Stop
							}
						}
					}
				}
			}
			if start >= 0 && end > start {
				// Extend to line boundaries
				for start > 0 && source[start-1] != '\n' {
					start--
				}
				for end < len(source) && source[end-1] != '\n' {
					end++
				}
				ranges = append(ranges, byteRange{start, end})
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	if len(ranges) == 0 {
		return md
	}
	// Build result excluding table ranges
	var buf bytes.Buffer
	pos := 0
	for _, r := range ranges {
		buf.Write(source[pos:r.start])
		buf.WriteString("📊 [Table]\n")
		pos = r.end
	}
	buf.Write(source[pos:])
	return strings.TrimSpace(buf.String())
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
		if entering {
			fmt.Fprintf(w, "<b>")
		} else {
			fmt.Fprintf(w, "</b>\n\n")
		}

	case *ast.Paragraph:
		if !entering {
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
		if entering {
			fmt.Fprintf(w, "<blockquote>")
		} else {
			fmt.Fprintf(w, "</blockquote>")
		}

	case *ast.FencedCodeBlock:
		if entering {
			var buf bytes.Buffer
			lines := node.Lines()
			for i := 0; i < lines.Len(); i++ {
				line := lines.At(i)
				buf.Write(line.Value(source))
			}
			fmt.Fprintf(w, "<pre><code>%s</code></pre>\n\n", ExpandTabs(EscapeHTML(strings.TrimRight(buf.String(), "\n"))))
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
			fmt.Fprintf(w, "<pre><code>%s</code></pre>\n\n", ExpandTabs(EscapeHTML(strings.TrimRight(buf.String(), "\n"))))
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

	case *ast.ListItem:
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

	case *extast.Table:
		if entering {
			var buf bytes.Buffer
			renderTableFallback(&buf, node, source)
			if buf.Len() > 0 {
				fmt.Fprintf(w, "<pre>%s</pre>\n\n", ExpandTabs(EscapeHTML(buf.String())))
			}
		}
		return ast.WalkSkipChildren, nil

	case *extast.TableHeader, *extast.TableRow, *extast.TableCell:
		// Handled by Table above (WalkSkipChildren)

	case *ast.ThematicBreak:
		if entering {
			fmt.Fprintf(w, "───────\n\n")
		}

	case *ast.Text:
		if entering {
			seg := node.Segment
			txt := EscapeHTML(string(seg.Value(source)))
			fmt.Fprintf(w, "%s", txt)
			if node.SoftLineBreak() || node.HardLineBreak() {
				fmt.Fprintf(w, "\n")
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
