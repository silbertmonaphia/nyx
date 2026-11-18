package kb

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRerankTextIncludesProvenancePrefix(t *testing.T) {
	passage := Passage{
		RelPath:     "B-Cook/diet.md",
		HeadingPath: []string{"煮食记", "碳水列表"},
		Content:     "一份碳水 + 一份蛋白",
	}
	assert.Equal(t, "B-Cook/diet.md › 煮食记 / 碳水列表\n\n一份碳水 + 一份蛋白", RerankText(passage))
}

func TestRerankTextWithoutProvenance(t *testing.T) {
	passage := Passage{Content: "just the body"}
	assert.Equal(t, "just the body", RerankText(passage))
}
