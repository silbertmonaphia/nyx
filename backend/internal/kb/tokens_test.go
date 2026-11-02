package kb

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEstimateTokensByScript(t *testing.T) {
	assert.Equal(t, 0, EstimateTokens(""))
	assert.Equal(t, 4, EstimateTokens("你好世界"))
	assert.Equal(t, 2, EstimateTokens("hello"))
	assert.Equal(t, 4, EstimateTokens("hello 世界"))
	assert.Greater(t, EstimateTokens("func main() { return 1 }"), 0)
	assert.Greater(t, EstimateTokens("package main\n\nfunc main() {}\n"), EstimateTokens("package main"))
}

func TestEstimateTokensMonotonic(t *testing.T) {
	samples := []string{
		"一段中文文本，包含标点。",
		"mixed Chinese and English text 混合文本",
		"func main() {\n\tprintln(\"hi\")\n}",
	}
	for _, sample := range samples {
		previous := 0
		runes := []rune(sample)
		for i := 1; i <= len(runes); i++ {
			current := EstimateTokens(string(runes[:i]))
			assert.GreaterOrEqual(t, current, previous, "prefix %q", string(runes[:i]))
			previous = current
		}
	}
}

func TestEstimateTokensCountsCJKPerRune(t *testing.T) {
	text := strings.Repeat("知识", 1000)
	assert.Equal(t, 2000, EstimateTokens(text))
}
