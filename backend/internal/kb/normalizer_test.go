package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func normalizeMarkdown(t *testing.T, raw string) *NormalizedDocument {
	t.Helper()
	n := NewMarkdownNormalizer()
	require.True(t, n.Handles(DocTypeMarkdown))
	require.True(t, n.Handles("md"))
	require.False(t, n.Handles("pdf"))
	doc, err := n.Normalize(context.Background(), []byte(raw), Source{})
	require.NoError(t, err)
	require.NotNil(t, doc)
	return doc
}

func TestMarkdownNormalizerNestedHeadings(t *testing.T) {
	doc := normalizeMarkdown(t, "# Top\n\nintro\n\n## Middle\n\nmiddle text\n")
	require.Len(t, doc.Blocks, 2)
	assert.Equal(t, "Top", doc.Title)
	assert.Equal(t, []string{"Top"}, doc.Blocks[0].HeadingPath)
	assert.Equal(t, "intro", doc.Blocks[0].Text)
	assert.Equal(t, []string{"Top", "Middle"}, doc.Blocks[1].HeadingPath)
	assert.Equal(t, "middle text", doc.Blocks[1].Text)
}

func TestMarkdownNormalizerFencedCodeStaysWhole(t *testing.T) {
	code := "```go\n" + strings.Join([]string{
		"line 1", "line 2", "line 3", "line 4", "line 5", "line 6",
		"line 7", "line 8", "line 9", "line 10", "line 11", "line 12",
	}, "\n") + "\n```\n"
	doc := normalizeMarkdown(t, code)
	require.Len(t, doc.Blocks, 1)
	assert.Equal(t, BlockCode, doc.Blocks[0].Kind)
	assert.Equal(t, 12, strings.Count(doc.Blocks[0].Text, "\n")+1)
	assert.NotContains(t, doc.Blocks[0].Text, "```")
}

func TestMarkdownNormalizerTableStaysTableBlock(t *testing.T) {
	doc := normalizeMarkdown(t, "| name | value |\n| --- | --- |\n| a | 1 |\n| b | 2 |\n")
	require.Len(t, doc.Blocks, 1)
	block := doc.Blocks[0]
	assert.Equal(t, BlockTable, block.Kind)
	require.NotNil(t, block.Table)
	assert.Equal(t, []string{"name", "value"}, block.Table.Header)
	require.Len(t, block.Table.Rows, 2)
	assert.Equal(t, []string{"a", "1"}, block.Table.Rows[0])
	assert.Contains(t, block.Text, "| name | value |")
}

func TestMarkdownNormalizerExcludesFrontMatter(t *testing.T) {
	doc := normalizeMarkdown(t, "---\ntitle: private\nsecret: true\n---\n# Visible\n\nbody\n")
	require.Len(t, doc.Blocks, 1)
	assert.Equal(t, []string{"Visible"}, doc.Blocks[0].HeadingPath)
	assert.Equal(t, "body", doc.Blocks[0].Text)
	for _, block := range doc.Blocks {
		assert.NotContains(t, block.Text, "private")
		assert.NotContains(t, block.Text, "secret")
	}
}

func TestMarkdownNormalizerImageAltBecomesText(t *testing.T) {
	doc := normalizeMarkdown(t, "![黄昏的庆云顶](https://example.com/img.jpg)\n")
	require.Len(t, doc.Blocks, 1)
	assert.Equal(t, "黄昏的庆云顶", doc.Blocks[0].Text)
	for _, block := range doc.Blocks {
		assert.NotContains(t, block.Text, "example.com")
	}
}

func TestMarkdownNormalizerImageWithoutAltContributesNothing(t *testing.T) {
	doc := normalizeMarkdown(t, "![](https://example.com/img.jpg)\n")
	assert.Empty(t, doc.Blocks)
}

func TestMarkdownNormalizerFileWithoutHeadings(t *testing.T) {
	doc := normalizeMarkdown(t, "first paragraph\n\nsecond paragraph\n")
	require.Len(t, doc.Blocks, 2)
	assert.Empty(t, doc.Blocks[0].HeadingPath)
	assert.Empty(t, doc.Blocks[1].HeadingPath)
}

func TestMarkdownNormalizerListItemsBecomeTextBlocks(t *testing.T) {
	doc := normalizeMarkdown(t, "## List\n\n- alpha\n- beta\n")
	require.GreaterOrEqual(t, len(doc.Blocks), 1)
	combined := ""
	for _, block := range doc.Blocks {
		combined += block.Text
	}
	assert.Contains(t, combined, "alpha")
	assert.Contains(t, combined, "beta")
}
