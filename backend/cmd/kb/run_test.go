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
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
	"nyx/internal/platform/config"
)

type fakeIndexService struct {
	registeredName string
	registeredRoot string
	registeredUser int64
	indexed        int
	result         kb.ScanResult
	err            error
}

func (f *fakeIndexService) RegisterSource(_ context.Context, userID int64, name, root string) (kb.Source, error) {
	f.registeredUser = userID
	f.registeredName = name
	f.registeredRoot = root
	if f.err != nil {
		return kb.Source{}, f.err
	}
	return kb.Source{ID: 1, Scope: kb.ScopeUser, OwnerID: userID, Name: name, Kind: kb.KindDir, RootPath: root}, nil
}

func (f *fakeIndexService) IndexSource(_ context.Context, src kb.Source) (kb.ScanResult, error) {
	f.indexed++
	return f.result, f.err
}

type fakeSharedStore struct {
	name    string
	root    string
	created bool
	onCall  func()
}

func (f *fakeSharedStore) UpsertSharedSource(_ context.Context, name, rootPath string) (kb.Source, error) {
	if f.onCall != nil {
		f.onCall()
	}
	f.name = name
	f.root = rootPath
	f.created = true
	return kb.Source{ID: 2, Scope: kb.ScopeShared, Name: name, Kind: kb.KindDir, RootPath: rootPath}, nil
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

func TestParseIndexOptions(t *testing.T) {
	valid, err := parseIndexOptions([]string{"--root", "/tmp/notes", "--shared"})
	require.NoError(t, err)
	assert.True(t, valid.Shared)
	assert.Equal(t, "/tmp/notes", valid.Root)

	user, err := parseIndexOptions([]string{"--root", "/tmp/notes", "--user", "42"})
	require.NoError(t, err)
	assert.Equal(t, int64(42), user.UserID)

	for _, args := range [][]string{
		{"--shared"},
		{"--root", "/tmp/notes"},
		{"--root", "/tmp/notes", "--shared", "--user", "42"},
		{"--root", "/tmp/notes", "--user", "0"},
	} {
		_, err := parseIndexOptions(args)
		assert.Error(t, err, strings.Join(args, " "))
	}
}

func TestRunIndexRefusesSharedWithoutExplicitFlag(t *testing.T) {
	_, err := parseIndexOptions([]string{"--root", t.TempDir()})
	require.Error(t, err)
	assert.ErrorIs(t, err, errChooseTarget)
	assert.Contains(t, err.Error(), "--shared")
}

func TestRunIndexSharedPrintsBeforeCreating(t *testing.T) {
	root := t.TempDir()
	service := &fakeIndexService{result: kb.ScanResult{Documents: 3, Chunks: 9}}
	shared := &fakeSharedStore{}
	var output bytes.Buffer
	shared.onCall = func() {
		assert.Contains(t, output.String(), "about to make")
		assert.Contains(t, output.String(), root)
		assert.Contains(t, output.String(), "visible to every user")
	}

	err := runIndexWith(context.Background(), indexOptions{
		Root: root, Shared: true, SourceName: "notes",
	}, &output, service, shared, &stubEmbedder{})
	require.NoError(t, err)
	assert.True(t, shared.created)
	assert.Equal(t, "notes", shared.name)
	assert.Equal(t, root, shared.root)
	assert.Equal(t, 1, service.indexed)
	assert.Contains(t, output.String(), "documents=3 chunks=9")
}

func TestRunIndexUserRegistersPrivateSource(t *testing.T) {
	root := t.TempDir()
	service := &fakeIndexService{}
	shared := &fakeSharedStore{}
	var output bytes.Buffer

	err := runIndexWith(context.Background(), indexOptions{
		Root: root, UserID: 42,
	}, &output, service, shared, &stubEmbedder{})
	require.NoError(t, err)
	assert.False(t, shared.created)
	assert.Equal(t, int64(42), service.registeredUser)
	assert.Equal(t, "private", service.registeredName)
	assert.Equal(t, root, service.registeredRoot)
	assert.Equal(t, 1, service.indexed)
}

func TestRunIndexWithoutEmbedderRefused(t *testing.T) {
	service := &fakeIndexService{}
	shared := &fakeSharedStore{}
	var output bytes.Buffer

	err := runIndexWith(context.Background(), indexOptions{
		Root: t.TempDir(), Shared: true, SourceName: "shared",
	}, &output, service, shared, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errEmbeddingConfig)
	assert.False(t, shared.created)
	assert.Equal(t, 0, service.indexed)
}

func TestDeploymentEmbedderNamesMissingConfiguration(t *testing.T) {
	_, err := deploymentEmbedder(&config.Config{LLMEnabled: false}, noop.NewTracerProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LLM_ENABLED")
	assert.Contains(t, err.Error(), "LLM_BASE_URL")
	assert.Contains(t, err.Error(), "LLM_EMBEDDING_BASE_URL")
}

func TestStatusReportsCorpusShape(t *testing.T) {
	store := &fakeStatusStore{
		sources: []kb.Source{
			{ID: 1, Scope: kb.ScopeShared, Name: "shared", Kind: kb.KindDir, Enabled: true},
			{ID: 2, Scope: kb.ScopeUser, OwnerID: 7, Name: "private", Kind: kb.KindDir, Enabled: true},
		},
		stats: map[int64]kb.SourceStats{
			1: {Documents: 12, Chunks: 40, Pending: 1},
			2: {Documents: 2, Chunks: 5, Pending: 0},
		},
	}
	var output bytes.Buffer
	require.NoError(t, runStatusWith(context.Background(), &output, store, "embed-model-x"))

	var report statusReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	assert.Equal(t, "embed-model-x", report.EmbeddingModel)
	require.Len(t, report.Sources, 2)
	assert.Equal(t, int64(12), report.Sources[0].Documents)
	assert.Equal(t, int64(40), report.Sources[0].Chunks)
	assert.Equal(t, int64(1), report.Sources[0].Pending)
	assert.Equal(t, "shared", report.Sources[0].Name)
	assert.Equal(t, "private", report.Sources[1].Name)
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
