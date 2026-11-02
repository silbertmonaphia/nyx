package kb

import (
	"unicode"
)

func EstimateTokens(s string) int {
	latin := 0
	tokens := 0
	for _, r := range s {
		switch {
		case isCJK(r):
			tokens++
		case r == '\n':
			tokens++
		case unicode.IsSpace(r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			latin++
		default:
			tokens++
		}
	}
	return tokens + (latin+3)/4
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
