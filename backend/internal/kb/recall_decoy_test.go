package kb_test

import (
	"context"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"nyx/internal/kb"
)

type kbRecallDecoyMode string

const (
	kbRecallDecoyNoise kbRecallDecoyMode = "noise"
	kbRecallDecoyNear  kbRecallDecoyMode = "near"
)

func kbRecallDecoyCountFromEnv() int {
	raw := os.Getenv("NYX_KB_RECALL_DECOYS")
	if raw == "" {
		return kbRecallDecoyCount
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return kbRecallDecoyCount
	}
	return n
}

func kbRecallNormalize(vec []float32) []float32 {
	sum := 0.0
	for _, value := range vec {
		sum += float64(value) * float64(value)
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(vec))
	if norm == 0 {
		return out
	}
	for i, value := range vec {
		out[i] = float32(float64(value) / norm)
	}
	return out
}

func kbRecallCosine(a, b []float32) float64 {
	dot := 0.0
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

func kbRecallOrthogonal(base []float32, rng *rand.Rand) []float32 {
	orth := make([]float32, len(base))
	for i := range orth {
		orth[i] = float32(rng.NormFloat64())
	}
	dot := 0.0
	for i := range orth {
		dot += float64(orth[i]) * float64(base[i])
	}
	for i := range orth {
		orth[i] = float32(float64(orth[i]) - dot*float64(base[i]))
	}
	return kbRecallNormalize(orth)
}

func kbRecallBuildDecoys(mode kbRecallDecoyMode, bases [][]float32, n int) ([][]float32, float64, float64) {
	rng := rand.New(rand.NewPCG(kbRecallDecoySeed, kbRecallDecoySeed)) //nolint:gosec // G404: deterministic PRNG keeps the decoys reproducible.
	out := make([][]float32, n)
	cosLo, cosHi := math.Inf(1), math.Inf(-1)
	for i := range out {
		base := kbRecallNormalize(bases[rng.IntN(len(bases))])
		vec := make([]float32, len(base))
		switch mode {
		case kbRecallDecoyNear:
			target := kbRecallDecoyCosLo + rng.Float64()*(kbRecallDecoyCosHi-kbRecallDecoyCosLo)
			orth := kbRecallOrthogonal(base, rng)
			sine := math.Sqrt(math.Max(0, 1-target*target))
			for j := range vec {
				vec[j] = float32(target*float64(base[j]) + sine*float64(orth[j]))
			}
			vec = kbRecallNormalize(vec)
		default:
			for j := range vec {
				vec[j] = float32(rng.NormFloat64())
			}
			vec = kbRecallNormalize(vec)
		}
		cosine := kbRecallCosine(vec, base)
		cosLo = math.Min(cosLo, cosine)
		cosHi = math.Max(cosHi, cosine)
		out[i] = vec
	}
	return out, cosLo, cosHi
}

func kbRecallVectorLiteral(vec []float32) string {
	var b strings.Builder
	b.Grow(len(vec) * 10)
	b.WriteByte('[')
	for i, value := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(value), 'g', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func kbRecallFixtureVectors(chunks []kbRecallChunk, vectors kbRecallVectorSet) [][]float32 {
	keys := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		keys = append(keys, chunk.Key)
	}
	sort.Strings(keys)
	out := make([][]float32, 0, len(keys))
	for _, key := range keys {
		out = append(out, vectors.Chunks[key].Vector)
	}
	return out
}

func kbRecallTopDirs(corpus kbRecallCorpus) []string {
	seen := make(map[string]struct{})
	for _, document := range corpus.Documents {
		parts := strings.SplitN(document.Path, "/", 2)
		if len(parts) == 2 {
			seen[parts[0]+"/"] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for dir := range seen {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

func kbRecallSeedDecoys(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *kb.SQLStore, owner int64, corpus kbRecallCorpus, chunks []kbRecallChunk, vectors kbRecallVectorSet, count int, mode kbRecallDecoyMode) (float64, float64) {
	t.Helper()
	require.Positive(t, count)
	sourceName := kbRecallDecoySourcePrefix + "_" + time.Now().Format("20060102_150405.000000000")
	src, err := store.UpsertDirSource(ctx, owner, sourceName, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kb_sources WHERE id = $1`, src.ID)
	})
	bases := kbRecallFixtureVectors(chunks, vectors)
	require.NotEmpty(t, bases)
	decoys, cosLo, cosHi := kbRecallBuildDecoys(mode, bases, count)
	dirs := kbRecallTopDirs(corpus)
	require.NotEmpty(t, dirs)
	for offset := 0; offset < count; offset += kbRecallDecoyBatch {
		end := min(offset+kbRecallDecoyBatch, count)
		relPaths := make([]string, 0, end-offset)
		literals := make([]string, 0, end-offset)
		for i := offset; i < end; i++ {
			relPaths = append(relPaths, dirs[i%len(dirs)]+"zz-decoy-"+strconv.Itoa(i)+".md")
			literals = append(literals, kbRecallVectorLiteral(decoys[i]))
		}
		_, err := pool.Exec(ctx, `
WITH docs AS (
    INSERT INTO kb_documents (
        source_id, owner_id, origin, rel_path, title, doc_type,
        content_hash, byte_size, mtime, status, error_detail, generation, content
    )
    SELECT $1, $2, 'file', p.rel_path, 'decoy', 'markdown',
           sha256(p.rel_path::bytea), 0, NULL, 'indexed', '', 1, NULL
    FROM unnest($3::text[]) AS p(rel_path)
    RETURNING id, rel_path
)
INSERT INTO kb_chunks (
    document_id, owner_id, source_id, generation, ordinal, heading_path,
    content, embed_text, content_hash, embedding, embedding_model, token_count, locator
)
SELECT d.id, $2, $1, 1, 0, ARRAY['decoy'],
       'decoy ' || p.rel_path, 'decoy ' || p.rel_path,
       sha256(p.rel_path::bytea), p.vec::vector, $6, 1, '{}'::jsonb
FROM unnest($4::text[], $5::text[]) AS p(rel_path, vec)
JOIN docs d ON d.rel_path = p.rel_path
`, src.ID, owner, relPaths, relPaths, literals, vectors.Model)
		require.NoError(t, err)
	}
	var rows int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*)
FROM kb_chunks c
JOIN kb_documents d ON d.id = c.document_id
JOIN kb_sources s ON s.id = c.source_id
WHERE s.id = $1
  AND c.embedding IS NOT NULL
  AND c.generation = d.generation
  AND c.embedding_model = $2
  AND d.generation > 0
  AND s.enabled
`, src.ID, vectors.Model).Scan(&rows))
	require.Equal(t, count, rows)
	return cosLo, cosHi
}

func kbRecallEligibleRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, owner int64, model string, sourceID *int64) int {
	t.Helper()
	statement := `
SELECT count(*)
FROM kb_chunks c
JOIN kb_documents d ON d.id = c.document_id
JOIN kb_sources s ON s.id = c.source_id
WHERE c.owner_id = $1
  AND d.owner_id = $1
  AND s.owner_id = $1
  AND c.embedding IS NOT NULL
  AND c.generation = d.generation
  AND c.embedding_model = $2
  AND d.generation > 0
  AND s.enabled`
	args := []any{owner, model}
	if sourceID != nil {
		statement += ` AND c.source_id = $3`
		args = append(args, *sourceID)
	}
	var rows int
	require.NoError(t, pool.QueryRow(ctx, statement, args...).Scan(&rows))
	return rows
}

func TestKbRecallDecoyCountFromEnv(t *testing.T) {
	t.Setenv("NYX_KB_RECALL_DECOYS", "123")
	require.Equal(t, 123, kbRecallDecoyCountFromEnv())
	t.Setenv("NYX_KB_RECALL_DECOYS", "0")
	require.Zero(t, kbRecallDecoyCountFromEnv())
	t.Setenv("NYX_KB_RECALL_DECOYS", "-1")
	require.Equal(t, kbRecallDecoyCount, kbRecallDecoyCountFromEnv())
	t.Setenv("NYX_KB_RECALL_DECOYS", "invalid")
	require.Equal(t, kbRecallDecoyCount, kbRecallDecoyCountFromEnv())
}

func TestKbRecallDecoyVectors(t *testing.T) {
	base := make([]float32, kbRecallDims)
	for i := range base {
		base[i] = float32(i%7) - 3
	}
	bases := [][]float32{base}

	nearA, loA, hiA := kbRecallBuildDecoys(kbRecallDecoyNear, bases, 20)
	nearB, loB, hiB := kbRecallBuildDecoys(kbRecallDecoyNear, bases, 20)
	require.Equal(t, nearA, nearB)
	require.Equal(t, loA, loB)
	require.Equal(t, hiA, hiB)
	require.GreaterOrEqual(t, loA, kbRecallDecoyCosLo-1e-4)
	require.LessOrEqual(t, hiA, kbRecallDecoyCosHi+1e-4)
	for _, vec := range nearA {
		require.Len(t, vec, kbRecallDims)
	}

	noiseA, _, _ := kbRecallBuildDecoys(kbRecallDecoyNoise, bases, 10)
	noiseB, _, _ := kbRecallBuildDecoys(kbRecallDecoyNoise, bases, 10)
	require.Equal(t, noiseA, noiseB)
	for _, vec := range noiseA {
		require.Len(t, vec, kbRecallDims)
	}
}
