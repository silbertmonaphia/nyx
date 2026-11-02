package kb

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func chunkOptions(target, maxTokens int) ChunkOptions {
	return ChunkOptions{TargetTokens: target, MaxTokens: maxTokens, MinTokens: 5, OverlapTokens: 0}
}

func TestChunkerNeverSpansTwoHeadingPaths(t *testing.T) {
	doc := &NormalizedDocument{Blocks: []Block{
		{HeadingPath: []string{"A"}, Kind: BlockText, Text: "alpha content"},
		{HeadingPath: []string{"B"}, Kind: BlockText, Text: "beta content"},
	}}
	specs := ChunkDocument("note.md", doc, chunkOptions(100, 200))
	require.Len(t, specs, 2)
	assert.Equal(t, []string{"A"}, specs[0].HeadingPath)
	assert.Equal(t, "alpha content", specs[0].Content)
	assert.Equal(t, []string{"B"}, specs[1].HeadingPath)
	assert.Equal(t, "beta content", specs[1].Content)
}

func TestChunkerRespectsHardCapForText(t *testing.T) {
	doc := &NormalizedDocument{}
	for i := 0; i < 12; i++ {
		doc.Blocks = append(doc.Blocks, Block{
			HeadingPath: []string{"Section"},
			Kind:        BlockText,
			Text:        strings.Repeat("这是一段用于验证分块硬上限的中文文本。", 30),
		})
	}
	specs := ChunkDocument("long.md", doc, chunkOptions(120, 200))
	require.NotEmpty(t, specs)
	for _, spec := range specs {
		assert.LessOrEqual(t, EstimateTokens(spec.EmbedText), 200, "chunk %d", spec.Ordinal)
	}
}

func TestChunkerKeepsOversizedFenceWhole(t *testing.T) {
	code := strings.Repeat("代码行内容\n", 80)
	doc := &NormalizedDocument{Blocks: []Block{
		{HeadingPath: []string{"Code"}, Kind: BlockCode, Text: code},
	}}
	specs := ChunkDocument("code.md", doc, chunkOptions(40, 50))
	require.Len(t, specs, 1)
	assert.Equal(t, code, specs[0].Content)
	assert.Greater(t, EstimateTokens(specs[0].EmbedText), 50)
}

func TestChunkerKeepsCodeWithAdjacentSection(t *testing.T) {
	code := strings.Repeat("x = 1\n", 40)
	doc := &NormalizedDocument{Blocks: []Block{
		{HeadingPath: []string{"Code"}, Kind: BlockText, Text: "before"},
		{HeadingPath: []string{"Code"}, Kind: BlockCode, Text: code},
	}}
	specs := ChunkDocument("code.md", doc, chunkOptions(20, 30))
	require.GreaterOrEqual(t, len(specs), 2)
	found := false
	for _, spec := range specs {
		if strings.Contains(spec.Content, "x = 1") {
			found = true
			assert.Equal(t, code, spec.Content)
		}
	}
	assert.True(t, found)
}

func TestChunkerSplitsOversizedTableByRowsWithHeader(t *testing.T) {
	table := &Table{Header: []string{"名称", "数值"}}
	for i := 0; i < 40; i++ {
		table.Rows = append(table.Rows, []string{"第七行内容", "数值内容"})
	}
	doc := &NormalizedDocument{Blocks: []Block{
		{HeadingPath: []string{"Table"}, Kind: BlockTable, Text: table.render(), Table: table},
	}}
	specs := ChunkDocument("table.md", doc, chunkOptions(30, 60))
	require.Greater(t, len(specs), 1)
	for _, spec := range specs {
		assert.True(t, strings.HasPrefix(spec.Content, "| 名称 | 数值 |"), "chunk %d content %q", spec.Ordinal, spec.Content)
		assert.Contains(t, spec.Content, "| --- | --- |")
	}
}

func TestChunkerKeepsShortSectionWithHeading(t *testing.T) {
	doc := &NormalizedDocument{Blocks: []Block{
		{HeadingPath: []string{"Long"}, Kind: BlockText, Text: strings.Repeat("长段落内容。", 40)},
		{HeadingPath: []string{"Tiny"}, Kind: BlockText, Text: "short line"},
	}}
	specs := ChunkDocument("short.md", doc, chunkOptions(50, 80))
	var tiny *ChunkSpec
	for i := range specs {
		if len(specs[i].HeadingPath) == 1 && specs[i].HeadingPath[0] == "Tiny" {
			tiny = &specs[i]
		}
	}
	require.NotNil(t, tiny)
	assert.Equal(t, "short line", tiny.Content)
	assert.Contains(t, tiny.EmbedText, "Tiny")
}

func TestChunkerEmbeddedTextCarriesProvenanceHeader(t *testing.T) {
	doc := &NormalizedDocument{Blocks: []Block{
		{
			HeadingPath: []string{"煮食记", "碳水列表"},
			Kind:        BlockText,
			Text:        "一份碳水 + 一份蛋白 + 两份蔬菜",
		},
	}}
	specs := ChunkDocument("B-Cook/2019-11-16-Cook-Diet.md", doc, chunkOptions(100, 200))
	require.Len(t, specs, 1)
	spec := specs[0]
	assert.True(t, strings.HasPrefix(spec.EmbedText, "B-Cook/2019-11-16-Cook-Diet.md › 煮食记 / 碳水列表\n\n"))
	assert.Equal(t, "一份碳水 + 一份蛋白 + 两份蔬菜", spec.Content)
	assert.NotContains(t, spec.Content, "B-Cook/2019-11-16-Cook-Diet.md")
	assert.NotContains(t, spec.Content, "煮食记")
}

func TestChunkerOrdinalsAreSequential(t *testing.T) {
	doc := &NormalizedDocument{Blocks: []Block{
		{HeadingPath: []string{"One"}, Kind: BlockText, Text: strings.Repeat("文本。", 100)},
		{HeadingPath: []string{"Two"}, Kind: BlockText, Text: strings.Repeat("内容。", 100)},
	}}
	specs := ChunkDocument("many.md", doc, chunkOptions(40, 60))
	require.Greater(t, len(specs), 2)
	for i, spec := range specs {
		assert.Equal(t, i, spec.Ordinal)
	}
}
