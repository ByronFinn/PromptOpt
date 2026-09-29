package web

import (
	"fmt"
	"html"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// reportCSS is the inline stylesheet of the standalone report.html
// export (self-contained download).
const reportCSS = ` :root{--fg:#1f2328;--dim:#6e7781;--line:#d8dee4;--bg:#f6f8fa}
 *{box-sizing:border-box}
 body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,"PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif;margin:0;padding:2rem 1.25rem;background:var(--bg);color:var(--fg);line-height:1.6}
 main{max-width:52rem;margin:0 auto;background:#fff;border:1px solid var(--line);border-radius:.5rem;padding:1.5rem 2rem}
 h1{font-size:1.3rem}h2{font-size:1.05rem;border-bottom:1px solid var(--line);padding-bottom:.25rem}
 table{border-collapse:collapse;width:100%;font-size:.85rem;margin:.75rem 0}
 th,td{padding:.35rem .6rem;border:1px solid var(--line);text-align:left}
 th{background:var(--bg)}
 pre{white-space:pre-wrap;word-break:break-word;background:var(--bg);border:1px solid var(--line);border-radius:.4rem;padding:.6rem .8rem;font-size:.8rem}
 p{white-space:pre-wrap}
 ul{padding-left:1.4rem}
`

// handleReportPage serves the in-site HTML view with download links.
func (s *Server) handleReportPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	src, err := readReport(runsReportPath(s.runsDir, id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The fragment is produced solely by renderMarkdownHTML with
	// html.EscapeString over every text byte, so template.HTML injects
	// it without re-escaping.
	render(w, "report.html", struct {
		ID   string
		Body template.HTML
	}{ID: id, Body: template.HTML(renderMarkdownHTML(src))})
}

// handleReportMarkdown serves report.md verbatim as an attachment.
func (s *Server) handleReportMarkdown(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	src, err := readReport(runsReportPath(s.runsDir, id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="report-`+id+`.md"`)
	_, _ = w.Write([]byte(src))
}

// handleReportStandalone serves a self-contained HTML document (inline
// CSS, no external assets) for offline viewing.
func (s *Server) handleReportStandalone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	src, err := readReport(runsReportPath(s.runsDir, id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body := renderMarkdownHTML(src)
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"zh\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<title>PromptOpt 报告 " + html.EscapeString(id) + "</title>\n")
	b.WriteString("<style>" + reportCSS + "</style>\n</head>\n<body>\n<main>\n")
	b.WriteString(body)
	b.WriteString("\n</main>\n</body>\n</html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="report-`+id+`.html"`)
	_, _ = w.Write([]byte(b.String()))
}

func runsReportPath(runsDir, id string) string {
	return filepath.Join(runsDir, id, "report.md")
}

func readReport(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// mdBlock is one parsed markdown block. Lines carries the payload:
// heading text, list items, raw table rows (separator rows dropped) or
// code lines (fence markers included).
type mdBlock struct {
	Kind  string // h1 | h2 | list | table | code | para
	Lines []string
}

// parseMarkdown blocks the report source tolerantly: H1/H2 headings,
// "- " bullet lists (two-space indented nested items flatten), pipe
// tables and ``` fences. A line matching no block structure joins the
// current paragraph verbatim — nothing is dropped, so a Top-1 prompt
// containing ``` lines that closes the report's own fence early still
// displays its spilled tail (as paragraphs or whatever block it forms)
// instead of vanishing. An unterminated fence flushes as a code block.
func parseMarkdown(src string) []mdBlock {
	var blocks []mdBlock
	var para []string
	var code []string // fence accumulator (local: the parser stays reentrant)
	flushPara := func() {
		if len(para) > 0 {
			blocks = append(blocks, mdBlock{Kind: "para", Lines: para})
			para = nil
		}
	}
	appendLines := func(kind string, lines []string) {
		if n := len(blocks); n > 0 && blocks[n-1].Kind == kind {
			blocks[n-1].Lines = append(blocks[n-1].Lines, lines...)
			return
		}
		flushPara()
		blocks = append(blocks, mdBlock{Kind: kind, Lines: lines})
	}
	inCode := false
	for raw := range strings.SplitSeq(strings.TrimSuffix(src, "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if inCode {
			if strings.HasPrefix(line, "```") {
				code = append(code, raw)
				blocks = append(blocks, mdBlock{Kind: "code", Lines: code})
				code = nil
				inCode = false
				continue
			}
			code = append(code, raw)
			continue
		}
		switch {
		case strings.HasPrefix(line, "```"):
			flushPara()
			inCode = true
			code = []string{line}
		case line == "":
			flushPara()
		case strings.HasPrefix(line, "# "):
			flushPara()
			blocks = append(blocks, mdBlock{Kind: "h1", Lines: []string{strings.TrimSpace(line[2:])}})
		case strings.HasPrefix(line, "## "):
			flushPara()
			blocks = append(blocks, mdBlock{Kind: "h2", Lines: []string{strings.TrimSpace(line[3:])}})
		case strings.HasPrefix(line, "- "):
			appendLines("list", []string{strings.TrimSpace(line[2:])})
		case isTableRow(line):
			if isTableSeparator(line) && len(blocks) > 0 && blocks[len(blocks)-1].Kind == "table" {
				continue // header/body separator, not data
			}
			appendLines("table", []string{line})
		default:
			para = append(para, line)
		}
	}
	flushPara()
	if inCode && len(code) > 0 {
		// Tolerant flush: an unterminated fence still renders its
		// content (as code) rather than dropping it.
		blocks = append(blocks, mdBlock{Kind: "code", Lines: code})
	}
	return blocks
}

// isTableRow reports whether the line looks like a pipe-table row.
func isTableRow(line string) bool {
	return strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|") && len(line) > 1
}

// isTableSeparator reports whether every cell is dashes/colons.
func isTableSeparator(line string) bool {
	cells := tableCells(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' && r != ':' {
				return false
			}
		}
	}
	return true
}

// tableCells splits one pipe row into trimmed cells.
func tableCells(line string) []string {
	t := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|"))
	var cells []string
	for c := range strings.SplitSeq(t, "|") {
		cells = append(cells, strings.TrimSpace(c))
	}
	return cells
}

// renderMarkdownHTML converts report.md source into a fragment whose
// every text byte passed through html.EscapeString — the renderer is
// the only HTML producer, so the fragment is safe to inject.
func renderMarkdownHTML(src string) string {
	var b strings.Builder
	for _, blk := range parseMarkdown(src) {
		switch blk.Kind {
		case "h1":
			fmt.Fprintf(&b, "<h1>%s</h1>\n", html.EscapeString(blk.Lines[0]))
		case "h2":
			fmt.Fprintf(&b, "<h2>%s</h2>\n", html.EscapeString(blockFirst(blk)))
		case "list":
			b.WriteString("<ul>\n")
			for _, it := range blk.Lines {
				fmt.Fprintf(&b, "<li>%s</li>\n", html.EscapeString(it))
			}
			b.WriteString("</ul>\n")
		case "code":
			b.WriteString("<pre><code>")
			for _, l := range blk.Lines {
				b.WriteString(html.EscapeString(l))
				b.WriteByte('\n')
			}
			b.WriteString("</code></pre>\n")
		case "table":
			renderTable(&b, blk.Lines)
		default:
			b.WriteString("<p>")
			for i, l := range blk.Lines {
				if i > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(html.EscapeString(l))
			}
			b.WriteString("</p>\n")
		}
	}
	return b.String()
}

func blockFirst(blk mdBlock) string {
	if len(blk.Lines) == 0 {
		return ""
	}
	return blk.Lines[0]
}

// renderTable emits the first row as the header row.
func renderTable(b *strings.Builder, rows []string) {
	if len(rows) == 0 {
		return
	}
	b.WriteString("<table>\n<thead>\n<tr>")
	for _, c := range tableCells(rows[0]) {
		fmt.Fprintf(b, "<th>%s</th>", html.EscapeString(c))
	}
	b.WriteString("</tr>\n</thead>\n<tbody>\n")
	for _, row := range rows[1:] {
		b.WriteString("<tr>")
		for _, c := range tableCells(row) {
			fmt.Fprintf(b, "<td>%s</td>", html.EscapeString(c))
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</tbody>\n</table>\n")
}
