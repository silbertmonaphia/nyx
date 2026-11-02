package kb

import (
	"context"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

const (
	frontMatterYAML = "---"
	frontMatterTOML = "+++"
)

type Normalizer interface {
	Handles(docType string) bool
	Normalize(ctx context.Context, raw []byte, src Source) (*NormalizedDocument, error)
}

type MarkdownNormalizer struct {
	parser goldmark.Markdown
}

func NewMarkdownNormalizer() *MarkdownNormalizer {
	return &MarkdownNormalizer{
		parser: goldmark.New(goldmark.WithExtensions(extension.Table)),
	}
}

func (n *MarkdownNormalizer) Handles(docType string) bool {
	switch strings.ToLower(docType) {
	case DocTypeMarkdown, "md":
		return true
	default:
		return false
	}
}

func (n *MarkdownNormalizer) Normalize(_ context.Context, raw []byte, _ Source) (*NormalizedDocument, error) {
	body, offset := stripFrontMatter(raw)
	doc := &NormalizedDocument{}
	root := n.parser.Parser().Parse(text.NewReader(body))
	n.walk(root, body, nil, offset, doc)
	return doc, nil
}

func (n *MarkdownNormalizer) walk(node ast.Node, src []byte, path []string, offset int, doc *NormalizedDocument) {
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch typed := child.(type) {
		case *ast.Heading:
			title := strings.TrimSpace(renderInlines(child, src))
			path = pushHeading(path, typed.Level, title)
			if typed.Level == 1 && doc.Title == "" {
				doc.Title = title
			}
		case *ast.Paragraph, *ast.TextBlock:
			body := strings.TrimSpace(renderInlines(child, src))
			if body == "" {
				continue
			}
			doc.Blocks = append(doc.Blocks, Block{
				HeadingPath: clonePath(path),
				Kind:        BlockText,
				Text:        body,
				Locator:     blockLocator(child, src, offset),
			})
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			doc.Blocks = append(doc.Blocks, codeBlock(child, src, path, offset))
		case *extast.Table:
			table := tableFromNode(typed, src)
			doc.Blocks = append(doc.Blocks, Block{
				HeadingPath: clonePath(path),
				Kind:        BlockTable,
				Text:        table.render(),
				Locator:     blockLocator(child, src, offset),
				Table:       table,
			})
		case *ast.List, *ast.Blockquote, *ast.ListItem:
			n.walk(child, src, path, offset, doc)
		}
	}
}

func codeBlock(node ast.Node, src []byte, path []string, offset int) Block {
	var b strings.Builder
	lines := node.Lines()
	for i := 0; i < lines.Len(); i++ {
		line := lines.At(i)
		b.Write(line.Value(src))
	}
	text := strings.TrimRight(b.String(), "\n")
	return Block{
		HeadingPath: clonePath(path),
		Kind:        BlockCode,
		Text:        text,
		Locator:     blockLocator(node, src, offset),
	}
}

func pushHeading(path []string, level int, text string) []string {
	if level < 1 {
		level = 1
	}
	if text == "" {
		return clonePath(path)
	}
	out := make([]string, 0, level)
	if len(path) >= level {
		out = append(out, path[:level-1]...)
	} else {
		out = append(out, path...)
	}
	return append(out, text)
}

func clonePath(path []string) []string {
	if len(path) == 0 {
		return nil
	}
	return append([]string(nil), path...)
}

func stripFrontMatter(raw []byte) ([]byte, int) {
	s := strings.TrimPrefix(string(raw), "\uFEFF")
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return raw, 0
	}
	first := strings.TrimSuffix(lines[0], "\r")
	delim := ""
	switch first {
	case frontMatterYAML:
		delim = frontMatterYAML
	case frontMatterTOML:
		delim = frontMatterTOML
	default:
		return []byte(s), 0
	}
	for i := 1; i < len(lines); i++ {
		probe := strings.TrimSuffix(lines[i], "\r")
		if probe == delim || (delim == frontMatterYAML && probe == "...") {
			return []byte(strings.Join(lines[i+1:], "\n")), i + 1
		}
	}
	return []byte(s), 0
}

func blockLocator(node ast.Node, src []byte, offset int) Locator {
	lines := node.Lines()
	if lines.Len() == 0 {
		return Locator{}
	}
	start := lines.At(0).Start
	end := lines.At(lines.Len() - 1).Stop
	return Locator{
		Line:    lineOf(src, start) + offset,
		EndLine: lineOf(src, end) + offset,
	}
}

func lineOf(src []byte, offset int) int {
	if offset > len(src) {
		offset = len(src)
	}
	if offset < 0 {
		offset = 0
	}
	return strings.Count(string(src[:offset]), "\n") + 1
}

func tableFromNode(node ast.Node, src []byte) *Table {
	t := &Table{}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch child.(type) {
		case *extast.TableHeader:
			for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
				t.Header = append(t.Header, cellText(cell, src))
			}
		case *extast.TableRow:
			row := make([]string, 0)
			for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
				row = append(row, cellText(cell, src))
			}
			t.Rows = append(t.Rows, row)
		}
	}
	return t
}

func cellText(node ast.Node, src []byte) string {
	text := strings.TrimSpace(renderInlines(node, src))
	return strings.Join(strings.Fields(text), " ")
}

func (t *Table) render() string {
	if t == nil || len(t.Header) == 0 {
		return ""
	}
	var b strings.Builder
	writeTableRow(&b, t.Header)
	b.WriteString("|")
	for range t.Header {
		b.WriteString(" --- |")
	}
	for _, row := range t.Rows {
		b.WriteByte('\n')
		writeTableRow(&b, row)
	}
	return b.String()
}

func writeTableRow(b *strings.Builder, cells []string) {
	b.WriteString("|")
	for _, cell := range cells {
		b.WriteString(" ")
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(cell, "|", "\\|"), "\n", " "))
		b.WriteString(" |")
	}
}

func renderInlines(node ast.Node, src []byte) string {
	var b strings.Builder
	renderInlinesInto(&b, node, src)
	return b.String()
}

func renderInlinesInto(b *strings.Builder, node ast.Node, src []byte) {
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch child := child.(type) {
		case *ast.Text:
			b.Write(child.Segment.Value(src))
			if child.HardLineBreak() || child.SoftLineBreak() {
				b.WriteByte('\n')
			}
		case *ast.String:
			b.Write(child.Value)
		case *ast.CodeSpan:
			for part := child.FirstChild(); part != nil; part = part.NextSibling() {
				if segment, ok := part.(*ast.Text); ok {
					b.Write(segment.Segment.Value(src))
				}
			}
		case *ast.Image:
			renderInlinesInto(b, child, src)
		case *ast.AutoLink:
			b.Write(child.Label(src))
		case *ast.RawHTML:
		case *ast.Link:
			renderInlinesInto(b, child, src)
		default:
			renderInlinesInto(b, child, src)
		}
	}
}
