package kb_test

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func kbRecallHitsAt(retrieved, relevant []string, n int) int {
	set := make(map[string]struct{}, len(relevant))
	for _, key := range relevant {
		set[key] = struct{}{}
	}
	if n > len(retrieved) {
		n = len(retrieved)
	}
	hits := 0
	for _, key := range retrieved[:n] {
		if _, ok := set[key]; ok {
			hits++
		}
	}
	return hits
}

func kbRecallFirstRank(retrieved, relevant []string) int {
	set := make(map[string]struct{}, len(relevant))
	for _, key := range relevant {
		set[key] = struct{}{}
	}
	for i, key := range retrieved {
		if _, ok := set[key]; ok {
			return i + 1
		}
	}
	return 0
}

func kbRecallScoreMetrics(retrieved, relevant []string) (recall1, recall5, recall10, hit5 float64, firstRank int) {
	if len(relevant) == 0 {
		return 0, 0, 0, 0, 0
	}
	den := float64(len(relevant))
	recall1 = float64(kbRecallHitsAt(retrieved, relevant, 1)) / den
	recall5 = float64(kbRecallHitsAt(retrieved, relevant, 5)) / den
	recall10 = float64(kbRecallHitsAt(retrieved, relevant, 10)) / den
	if kbRecallHitsAt(retrieved, relevant, 5) > 0 {
		hit5 = 1
	}
	firstRank = kbRecallFirstRank(retrieved, relevant)
	return recall1, recall5, recall10, hit5, firstRank
}

type kbRecallVacuousQuery struct {
	Key      string
	Path     string
	Eligible int
	TopK     int
}

func kbRecallVacuousQueries(corpus kbRecallCorpus, chunks []kbRecallChunk, topK int) []kbRecallVacuousQuery {
	out := make([]kbRecallVacuousQuery, 0)
	for _, query := range corpus.Queries {
		if query.Category == kbRecallCategoryNegative {
			continue
		}
		eligible := 0
		for _, chunk := range chunks {
			if query.Path == "" || strings.HasPrefix(chunk.RelPath, query.Path) {
				eligible++
			}
		}
		if eligible <= topK {
			out = append(out, kbRecallVacuousQuery{
				Key:      query.Key,
				Path:     query.Path,
				Eligible: eligible,
				TopK:     topK,
			})
		}
	}
	return out
}

type kbRecallCategoryShortfall struct {
	Category string
	Count    int
	Minimum  int
}

func kbRecallQueryCounts(corpus kbRecallCorpus) (labelled, negatives int) {
	for _, query := range corpus.Queries {
		if query.Category == kbRecallCategoryNegative {
			negatives++
			continue
		}
		labelled++
	}
	return labelled, negatives
}

func kbRecallNearMissCount(corpus kbRecallCorpus) int {
	count := 0
	for _, query := range corpus.Queries {
		if query.NearMiss {
			count++
		}
	}
	return count
}

func kbRecallCategoryShortfalls(corpus kbRecallCorpus, minimums map[string]int) []kbRecallCategoryShortfall {
	counts := make(map[string]int, len(minimums))
	for _, query := range corpus.Queries {
		if query.Category == kbRecallCategoryNegative {
			continue
		}
		counts[query.Category]++
	}
	categories := make([]string, 0, len(minimums))
	for category := range minimums {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	shortfalls := make([]kbRecallCategoryShortfall, 0)
	for _, category := range categories {
		minimum := minimums[category]
		if counts[category] < minimum {
			shortfalls = append(shortfalls, kbRecallCategoryShortfall{
				Category: category,
				Count:    counts[category],
				Minimum:  minimum,
			})
		}
	}
	return shortfalls
}

type kbRecallPrereqActionKind string

const (
	kbRecallPrereqRun  kbRecallPrereqActionKind = "run"
	kbRecallPrereqSkip kbRecallPrereqActionKind = "skip"
	kbRecallPrereqFail kbRecallPrereqActionKind = "fail"
)

func kbRecallPrereqAction(ci, skipContainers bool, dbURL string) kbRecallPrereqActionKind {
	if skipContainers {
		return kbRecallPrereqSkip
	}
	if dbURL == "" {
		if ci {
			return kbRecallPrereqFail
		}
		return kbRecallPrereqSkip
	}
	return kbRecallPrereqRun
}

func kbRecallCIEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CI"))) {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}

func kbRecallDuplicateKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	duplicates := make([]string, 0)
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			duplicates = append(duplicates, key)
			continue
		}
		seen[key] = struct{}{}
	}
	return duplicates
}

func TestKbRecallScoreMetrics(t *testing.T) {
	retrieved := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

	recall1, recall5, recall10, hit5, firstRank := kbRecallScoreMetrics(retrieved, []string{"g"})
	require.Zero(t, recall1)
	require.Zero(t, recall5)
	require.Equal(t, 1.0, recall10)
	require.Zero(t, hit5)
	require.Equal(t, 7, firstRank)

	recall1, recall5, recall10, hit5, firstRank = kbRecallScoreMetrics(retrieved, []string{"a", "g"})
	require.Equal(t, 0.5, recall1)
	require.Equal(t, 0.5, recall5)
	require.Equal(t, 1.0, recall10)
	require.Equal(t, 1.0, hit5)
	require.Equal(t, 1, firstRank)

	recall1, recall5, recall10, hit5, firstRank = kbRecallScoreMetrics(retrieved, []string{"k"})
	require.Zero(t, recall1)
	require.Zero(t, recall5)
	require.Zero(t, recall10)
	require.Zero(t, hit5)
	require.Zero(t, firstRank)
}

func TestKbRecallCategoryFloors(t *testing.T) {
	minimums := kbRecallCategoryMinimums()
	corpus := kbRecallCorpus{}
	for category, count := range minimums {
		for i := 0; i < count; i++ {
			corpus.Queries = append(corpus.Queries, kbRecallQuery{Category: category})
		}
	}
	require.Empty(t, kbRecallCategoryShortfalls(corpus, minimums))

	labelled, negatives := kbRecallQueryCounts(corpus)
	require.Equal(t, 34, labelled)
	require.Zero(t, negatives)

	for i, query := range corpus.Queries {
		if query.Category == kbRecallCategorySemantic {
			corpus.Queries = append(corpus.Queries[:i], corpus.Queries[i+1:]...)
			break
		}
	}
	shortfalls := kbRecallCategoryShortfalls(corpus, minimums)
	require.Len(t, shortfalls, 1)
	require.Equal(t, kbRecallCategorySemantic, shortfalls[0].Category)
	require.Equal(t, 5, shortfalls[0].Count)
	require.Equal(t, 6, shortfalls[0].Minimum)

	counted := kbRecallCorpus{}
	for i := 0; i < 55; i++ {
		counted.Queries = append(counted.Queries, kbRecallQuery{Category: kbRecallCategorySemantic})
	}
	for i := 0; i < 12; i++ {
		counted.Queries = append(counted.Queries, kbRecallQuery{Category: kbRecallCategoryNegative})
	}
	labelled, negatives = kbRecallQueryCounts(counted)
	require.Equal(t, 55, labelled)
	require.Equal(t, 12, negatives)
}

func TestKbRecallNearMissCount(t *testing.T) {
	corpus := kbRecallCorpus{Queries: []kbRecallQuery{
		{Key: "a", Category: kbRecallCategoryNegative, NearMiss: true},
		{Key: "b", Category: kbRecallCategoryNegative, NearMiss: true},
		{Key: "c", Category: kbRecallCategoryNegative, NearMiss: true},
		{Key: "d", Category: kbRecallCategoryNegative, NearMiss: true},
		{Key: "e", Category: kbRecallCategoryNegative},
		{Key: "f", Category: kbRecallCategorySemantic},
	}}
	require.Equal(t, 4, kbRecallNearMissCount(corpus))
	labelled, negatives := kbRecallQueryCounts(corpus)
	require.Equal(t, 1, labelled)
	require.Equal(t, 5, negatives)
}

func TestKbRecallCandidateSets(t *testing.T) {
	corpus := kbRecallCorpus{
		Queries: []kbRecallQuery{
			{Key: "q-path", Category: kbRecallCategoryPath, Path: "B-Sleep/", Relevant: []string{"B-Sleep/a.md::a"}},
			{Key: "q-negative", Category: kbRecallCategoryNegative},
		},
	}
	chunks := make([]kbRecallChunk, 0, 6)
	for i := 0; i < 5; i++ {
		chunks = append(chunks, kbRecallChunk{RelPath: "B-Sleep/a.md"})
	}

	vacuous := kbRecallVacuousQueries(corpus, chunks, kbRecallTopK)
	require.Len(t, vacuous, 1)
	require.Equal(t, "q-path", vacuous[0].Key)
	require.Equal(t, 5, vacuous[0].Eligible)

	chunks = append(chunks, kbRecallChunk{RelPath: "B-Sleep/b.md"})
	require.Empty(t, kbRecallVacuousQueries(corpus, chunks, kbRecallTopK))
}

func TestKbRecallCIEnabled(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"true":  true,
		"1":     true,
		"yes":   true,
	}
	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CI", value)
			require.Equal(t, want, kbRecallCIEnabled())
		})
	}
}

func TestKbRecallDuplicateKeys(t *testing.T) {
	require.Empty(t, kbRecallDuplicateKeys([]string{"a", "b"}))
	require.Equal(t, []string{"a"}, kbRecallDuplicateKeys([]string{"a", "b", "a"}))
	require.Equal(t, []string{"a", "a"}, kbRecallDuplicateKeys([]string{"a", "a", "a"}))
}

func TestKbRecallPrereqAction(t *testing.T) {
	url := "postgres://postgres:postgres@127.0.0.1:5433/nyx?sslmode=disable"
	cases := []struct {
		name           string
		ci             bool
		skipContainers bool
		dbURL          string
		want           kbRecallPrereqActionKind
	}{
		{name: "local without db url", dbURL: "", want: kbRecallPrereqSkip},
		{name: "local with db url", dbURL: url, want: kbRecallPrereqRun},
		{name: "ci without db url", ci: true, dbURL: "", want: kbRecallPrereqFail},
		{name: "ci with db url", ci: true, dbURL: url, want: kbRecallPrereqRun},
		{name: "local skip containers", skipContainers: true, dbURL: url, want: kbRecallPrereqSkip},
		{name: "ci skip containers", ci: true, skipContainers: true, dbURL: url, want: kbRecallPrereqSkip},
		{name: "ci coverage gate opt out", ci: true, skipContainers: true, dbURL: url, want: kbRecallPrereqSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, kbRecallPrereqAction(tc.ci, tc.skipContainers, tc.dbURL))
		})
	}
}
