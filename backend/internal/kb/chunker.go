package kb

import (
	"crypto/sha256"
	"strings"
)

const (
	defaultChunkTarget  = 400
	defaultChunkMax     = 700
	defaultChunkMin     = 50
	defaultChunkOverlap = 80
)

type headingGroup struct {
	path   []string
	blocks []Block
}

type atom struct {
	text    string
	kind    string
	locator Locator
}

type packedChunk struct {
	text    string
	locator Locator
}

func ChunkDocument(relPath string, doc *NormalizedDocument, opts ChunkOptions) []ChunkSpec {
	opts = normalizeChunkOptions(opts)
	if doc == nil || len(doc.Blocks) == 0 {
		return nil
	}
	var out []ChunkSpec
	ordinal := 0
	for _, group := range groupByHeading(doc.Blocks) {
		prefix := chunkPrefix(relPath, group.path)
		prefixTokens := EstimateTokens(prefix)
		maxBody := opts.MaxTokens - prefixTokens
		if maxBody < 1 {
			maxBody = 1
		}
		targetBody := opts.TargetTokens - prefixTokens
		if targetBody < 1 {
			targetBody = 1
		}
		if targetBody > maxBody {
			targetBody = maxBody
		}
		atoms := atomsForBlocks(group.blocks, maxBody)
		for _, packed := range packAtoms(atoms, targetBody, maxBody, opts.OverlapTokens) {
			out = append(out, ChunkSpec{
				Ordinal:     ordinal,
				HeadingPath: clonePath(group.path),
				Content:     packed.text,
				EmbedText:   prefix + packed.text,
				ContentHash: hashText(packed.text),
				TokenCount:  EstimateTokens(packed.text),
				Locator:     packed.locator,
			})
			ordinal++
		}
	}
	return out
}

func normalizeChunkOptions(opts ChunkOptions) ChunkOptions {
	if opts.TargetTokens <= 0 {
		opts.TargetTokens = defaultChunkTarget
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = defaultChunkMax
	}
	if opts.MaxTokens < opts.TargetTokens {
		opts.MaxTokens = opts.TargetTokens
	}
	if opts.MinTokens <= 0 {
		opts.MinTokens = defaultChunkMin
	}
	if opts.OverlapTokens < 0 {
		opts.OverlapTokens = defaultChunkOverlap
	}
	if opts.OverlapTokens >= opts.TargetTokens {
		opts.OverlapTokens = opts.TargetTokens / 4
	}
	return opts
}

func groupByHeading(blocks []Block) []headingGroup {
	groups := make([]headingGroup, 0)
	lastKey := ""
	for i, block := range blocks {
		key := strings.Join(block.HeadingPath, "\x00")
		if i == 0 || key != lastKey {
			groups = append(groups, headingGroup{path: clonePath(block.HeadingPath)})
			lastKey = key
		}
		groups[len(groups)-1].blocks = append(groups[len(groups)-1].blocks, block)
	}
	return groups
}

func atomsForBlocks(blocks []Block, maxTokens int) []atom {
	var atoms []atom
	for _, block := range blocks {
		switch block.Kind {
		case BlockCode:
			atoms = append(atoms, atom{text: block.Text, kind: BlockCode, locator: block.Locator})
		case BlockTable:
			atoms = append(atoms, tableAtoms(block, maxTokens)...)
		default:
			for _, piece := range splitText(block.Text, maxTokens) {
				atoms = append(atoms, atom{text: piece, kind: BlockText, locator: block.Locator})
			}
		}
	}
	return atoms
}

func tableAtoms(block Block, maxTokens int) []atom {
	table := block.Table
	if table == nil || len(table.Header) == 0 {
		return []atom{{text: block.Text, kind: BlockTable, locator: block.Locator}}
	}
	full := table.render()
	if EstimateTokens(full) <= maxTokens {
		return []atom{{text: full, kind: BlockTable, locator: block.Locator}}
	}
	header := tableHeaderLines(table)
	var atoms []atom
	rows := make([]string, 0, len(table.Rows))
	startRow := 0
	tokens := EstimateTokens(header)
	flush := func(endRow int) {
		if len(rows) == 0 {
			return
		}
		locator := block.Locator
		locator.TableRowStart = startRow + 1
		locator.TableRowEnd = endRow
		atoms = append(atoms, atom{
			text:    header + "\n" + strings.Join(rows, "\n"),
			kind:    BlockTable,
			locator: locator,
		})
		rows = nil
		tokens = EstimateTokens(header)
	}
	for i, row := range table.Rows {
		line := tableRowLine(row)
		lineTokens := EstimateTokens(line)
		if len(rows) > 0 && tokens+lineTokens > maxTokens {
			flush(i)
			startRow = i
		}
		if len(rows) == 0 {
			startRow = i
		}
		rows = append(rows, line)
		tokens += lineTokens
	}
	flush(len(table.Rows))
	return atoms
}

func tableHeaderLines(table *Table) string {
	return tableRowLine(table.Header) + "\n|" + strings.Repeat(" --- |", len(table.Header))
}

func tableRowLine(cells []string) string {
	var b strings.Builder
	b.WriteString("|")
	for _, cell := range cells {
		b.WriteString(" ")
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(cell, "|", "\\|"), "\n", " "))
		b.WriteString(" |")
	}
	return b.String()
}

func splitText(text string, maxTokens int) []string {
	if EstimateTokens(text) <= maxTokens {
		return []string{text}
	}
	sentences := splitSentences(text)
	var out []string
	for _, sentence := range sentences {
		sentence = strings.TrimSpace(sentence)
		if sentence == "" {
			continue
		}
		if EstimateTokens(sentence) <= maxTokens {
			out = append(out, sentence)
			continue
		}
		out = append(out, hardSplit(sentence, maxTokens)...)
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

func splitSentences(text string) []string {
	var out []string
	var b strings.Builder
	for _, r := range text {
		b.WriteRune(r)
		if r == '\n' || r == '。' || r == '！' || r == '？' || r == '；' || r == '.' || r == '!' || r == '?' || r == ';' {
			out = append(out, b.String())
			b.Reset()
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func hardSplit(text string, maxTokens int) []string {
	var out []string
	var b strings.Builder
	for _, r := range text {
		b.WriteRune(r)
		if EstimateTokens(b.String()) >= maxTokens {
			out = append(out, b.String())
			b.Reset()
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

//nolint:gocyclo // Packing, overlap and the code-block exception are one loop.
func packAtoms(atoms []atom, targetTokens, maxTokens, overlapTokens int) []packedChunk {
	var out []packedChunk
	current := make([]string, 0, len(atoms))
	currentTokens := 0
	locator := Locator{}
	overlap := ""
	flush := func() {
		if len(current) == 0 {
			return
		}
		text := strings.Join(current, "\n\n")
		out = append(out, packedChunk{text: text, locator: locator})
		overlap = tailTokens(text, overlapTokens)
		current = nil
		currentTokens = 0
		locator = Locator{}
	}
	for _, piece := range atoms {
		pieceTokens := EstimateTokens(piece.text)
		if piece.kind == BlockCode && pieceTokens > maxTokens {
			flush()
			out = append(out, packedChunk{text: piece.text, locator: piece.locator})
			overlap = ""
			continue
		}
		lead := ""
		leadTokens := 0
		if len(current) == 0 && overlap != "" && piece.kind == BlockText {
			if EstimateTokens(overlap)+pieceTokens <= maxTokens {
				lead = overlap
				leadTokens = EstimateTokens(overlap)
			}
		}
		if len(current) > 0 && currentTokens+pieceTokens > targetTokens {
			flush()
			if overlap != "" && piece.kind == BlockText && EstimateTokens(overlap)+pieceTokens <= maxTokens {
				lead = overlap
				leadTokens = EstimateTokens(overlap)
			}
		}
		if len(current) == 0 {
			locator = piece.locator
		} else {
			locator.EndLine = piece.locator.EndLine
		}
		if lead != "" {
			current = append(current, lead)
			currentTokens += leadTokens
		}
		current = append(current, piece.text)
		currentTokens += pieceTokens
	}
	flush()
	return out
}

func tailTokens(text string, limit int) string {
	if limit <= 0 || text == "" {
		return ""
	}
	runes := []rune(text)
	for i := len(runes) - 1; i >= 0; i-- {
		if EstimateTokens(string(runes[i:])) > limit {
			if i == len(runes)-1 {
				return ""
			}
			return string(runes[i+1:])
		}
	}
	return text
}

func chunkPrefix(relPath string, headingPath []string) string {
	parts := make([]string, 0, 2)
	if relPath != "" {
		parts = append(parts, relPath)
	}
	if len(headingPath) > 0 {
		parts = append(parts, strings.Join(headingPath, " / "))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " › ") + "\n\n"
}

func hashText(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return sum[:]
}
