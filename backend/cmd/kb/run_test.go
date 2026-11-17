package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/kb"
	"nyx/internal/llm"
	"nyx/internal/llmsettings"
	"nyx/internal/platform/config"
)

type fakeIndexService struct {
	indexed int
	result  kb.ScanResult
	err     error
}

func (f *fakeIndexService) IndexSource(_ context.Context, src kb.Source) (kb.ScanResult, error) {
	f.indexed++
	return f.result, f.err
}

type fakeDirStore struct {
	ownerID int64
	name    string
	root    string
	created bool
}

func (f *fakeDirStore) UpsertDirSource(_ context.Context, ownerID int64, name, rootPath string) (kb.Source, error) {
	f.ownerID = ownerID
	f.name = name
	f.root = rootPath
	f.created = true
	return kb.Source{ID: 2, OwnerID: ownerID, Name: name, Kind: kb.KindDir, RootPath: rootPath, Enabled: true}, nil
}

type fakeStatusStore struct {
	sources []kb.Source
	stats   map[int64]kb.SourceStats
}

func (f *fakeStatusStore) ListAllSources(_ context.Context) ([]kb.Source, error) {
	return f.sources, nil
}

func (f *fakeStatusStore) SourceStats(_ context.Context, id int64) (kb.SourceStats, error) {
	return f.stats[id], nil
}

type fakeResolver struct {
	stack *llmsettings.UserStack
	err   error
}

func (f *fakeResolver) Resolve(_ context.Context, _ int) (*llmsettings.UserStack, error) {
	return f.stack, f.err
}

type stubProvider struct{}

func (stubProvider) Chat(_ context.Context, _ llm.ChatRequest, _ func(string, *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	return nil, nil
}

func (stubProvider) Embed(_ context.Context, _ llm.EmbedRequest) ([][]float32, error) {
	return nil, nil
}

func TestParseIndexOptions(t *testing.T) {
	valid, err := parseIndexOptions([]string{"--user", "42", "--root", "/tmp/notes", "--name", "notes"})
	require.NoError(t, err)
	assert.Equal(t, int64(42), valid.UserID)
	assert.Equal(t, "/tmp/notes", valid.Root)
	assert.Equal(t, "notes", valid.Name)

	unnamed, err := parseIndexOptions([]string{"--user", "42", "--root", "/tmp/notes"})
	require.NoError(t, err)
	assert.Empty(t, unnamed.Name)

	for _, args := range [][]string{
		{"--root", "/tmp/notes"},
		{"--user", "42"},
		{"--user", "abc", "--root", "/tmp/notes"},
		{"--user", "0", "--root", "/tmp/notes"},
		{"--user", "-1", "--root", "/tmp/notes"},
		{"--user", "42", "--root", "/tmp/notes", "--name", "inline"},
		{"--user", "42", "--root", "/tmp/notes", "--name", "INLINE"},
		{"--user", "42", "--root", "/tmp/notes", "--name", strings.Repeat("x", 201)},
	} {
		_, err := parseIndexOptions(args)
		assert.Error(t, err, strings.Join(args, " "))
	}
}

func TestRunIndexRegistersUserSource(t *testing.T) {
	root := t.TempDir()
	service := &fakeIndexService{result: kb.ScanResult{Documents: 3, Chunks: 9}}
	store := &fakeDirStore{}
	var output bytes.Buffer

	err := runIndexWith(context.Background(), indexOptions{
		UserID: 42, Root: root, Name: "notes",
	}, &output, service, store, stubEmbedder{})
	require.NoError(t, err)
	assert.True(t, store.created)
	assert.Equal(t, int64(42), store.ownerID)
	assert.Equal(t, "notes", store.name)
	assert.Equal(t, root, store.root)
	assert.Equal(t, 1, service.indexed)
	assert.Contains(t, output.String(), "indexing user=42 source=\"notes\"")
	assert.Contains(t, output.String(), "documents=3 chunks=9")
}

func TestRunIndexDefaultsNameFromDirectory(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "posts_douban")
	require.NoError(t, os.Mkdir(root, 0o700))
	store := &fakeDirStore{}
	var output bytes.Buffer

	err := runIndexWith(context.Background(), indexOptions{
		UserID: 7, Root: root,
	}, &output, &fakeIndexService{}, store, stubEmbedder{})
	require.NoError(t, err)
	assert.Equal(t, "posts_douban", store.name)
}

func TestRunIndexRejectsReservedDirectoryName(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "inline")
	require.NoError(t, os.Mkdir(root, 0o700))
	store := &fakeDirStore{}

	err := runIndexWith(context.Background(), indexOptions{
		UserID: 7, Root: root,
	}, &bytes.Buffer{}, &fakeIndexService{}, store, stubEmbedder{})
	require.Error(t, err)
	assert.False(t, store.created)
}

func TestRunIndexWithoutEmbedderRefused(t *testing.T) {
	service := &fakeIndexService{}
	store := &fakeDirStore{}
	var output bytes.Buffer

	err := runIndexWith(context.Background(), indexOptions{
		UserID: 42, Root: t.TempDir(), Name: "notes",
	}, &output, service, store, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errEmbeddingConfig)
	assert.False(t, store.created)
	assert.Equal(t, 0, service.indexed)
}

func TestResolveUserEmbedder(t *testing.T) {
	cfg := &config.Config{EmbeddingDimensions: 1024}

	_, err := resolveUserEmbedder(context.Background(), cfg, &fakeResolver{err: llmsettings.ErrNotFound}, 42)
	require.Error(t, err)
	assert.ErrorIs(t, err, errEmbeddingConfig)

	_, err = resolveUserEmbedder(context.Background(), cfg, &fakeResolver{stack: &llmsettings.UserStack{}}, 42)
	require.Error(t, err)
	assert.ErrorIs(t, err, errEmbeddingConfig)

	embedder, err := resolveUserEmbedder(context.Background(), cfg, &fakeResolver{
		stack: &llmsettings.UserStack{Embed: stubProvider{}, EmbedModel: "embed-x"},
	}, 42)
	require.NoError(t, err)
	assert.Equal(t, "embed-x", embedder.Model())
}

func TestStatusReportsOwnerAndKind(t *testing.T) {
	store := &fakeStatusStore{
		sources: []kb.Source{
			{ID: 1, OwnerID: 7, Name: "posts_douban", Kind: kb.KindDir, Enabled: true},
			{ID: 2, OwnerID: 7, Name: "inline", Kind: kb.KindInline, Enabled: false},
		},
		stats: map[int64]kb.SourceStats{
			1: {Documents: 12, Chunks: 40, Pending: 1},
			2: {Documents: 2, Chunks: 5, Pending: 0},
		},
	}
	var output bytes.Buffer
	require.NoError(t, runStatusWith(context.Background(), &output, store))

	var report statusReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	require.Len(t, report.Sources, 2)
	assert.Equal(t, int64(7), report.Sources[0].OwnerID)
	assert.Equal(t, "posts_douban", report.Sources[0].Name)
	assert.Equal(t, int64(12), report.Sources[0].Documents)
	assert.Equal(t, int64(40), report.Sources[0].Chunks)
	assert.Equal(t, int64(1), report.Sources[0].Pending)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(output.Bytes(), &raw))
	assert.NotContains(t, output.String(), "\"scope\"")
	assert.NotContains(t, output.String(), "embedding_model")
}

func TestResolveRootRequiresDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file.md")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err := resolveRoot(file)
	require.Error(t, err)

	_, err = resolveRoot(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

type stubEmbedder struct{}

func (stubEmbedder) EmbedStrings(_ context.Context, inputs []string) ([][]float32, error) {
	return make([][]float32, len(inputs)), nil
}

func (stubEmbedder) Model() string { return "stub-model" }

var _ embeddingResolver = (*fakeResolver)(nil)
