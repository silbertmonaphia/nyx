package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

type handlerEnv struct {
	store   *fakeStore
	service *Service
	server  *httptest.Server
	tokenA  string
	tokenB  string
}

type testResponse struct {
	StatusCode int
	BodyBytes  []byte
}

func (e *handlerEnv) request(t *testing.T, method, path, token string, body io.Reader, contentType string) testResponse {
	t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, body)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return testResponse{StatusCode: resp.StatusCode, BodyBytes: raw}
}

func (e *handlerEnv) jsonRequest(t *testing.T, method, path, token string, payload any) testResponse {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(raw)
	}
	return e.request(t, method, path, token, body, "application/json")
}

func setupHandler(t *testing.T, cfg Config, embedder Embedder) *handlerEnv {
	t.Helper()
	store := newFakeStore()
	service := newTestService(store, embedder, cfg)
	tokens, err := auth.NewTokenService([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute)
	require.NoError(t, err)
	tokenA, err := tokens.GenerateToken(1, "alice")
	require.NoError(t, err)
	tokenB, err := tokens.GenerateToken(2, "bob")
	require.NoError(t, err)

	api.OverrideHumaErrors()
	router := chi.NewMux()
	router.Use(middleware.StoreRequest)
	hapi := humachi.New(router, huma.DefaultConfig("Nyx test", "0.0.0"))
	handler := NewHandler(service)
	RegisterKBOpsTest(hapi, handler, tokens, true)
	RegisterKBUploadRoute(router, handler, tokens)

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &handlerEnv{store: store, service: service, server: server, tokenA: tokenA, tokenB: tokenB}
}

func decodeBody[T any](t *testing.T, resp testResponse) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(resp.BodyBytes, &out))
	return out
}

func bodyString(t *testing.T, resp testResponse) string {
	t.Helper()
	return string(resp.BodyBytes)
}

func multipartUpload(t *testing.T, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return &buffer, writer.FormDataContentType()
}

func TestHandlerSourceLifecycle(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "alice")
	require.NoError(t, os.MkdirAll(root, 0o750))
	env := setupHandler(t, testConfig(base), &countingEmbedder{})

	resp := env.jsonRequest(t, http.MethodGet, "/api/kb/sources", env.tokenA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := decodeBody[struct {
		Sources []SourceView `json:"sources"`
	}](t, resp)
	assert.Empty(t, list.Sources)

	resp = env.jsonRequest(t, http.MethodPost, "/api/kb/sources", env.tokenA, map[string]string{
		"name": "mine", "root_path": root,
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeBody[SourceView](t, resp)
	assert.Equal(t, "mine", created.Name)
	assert.Equal(t, ScopeUser, created.Scope)
	assert.Equal(t, KindDir, created.Kind)

	resp = env.jsonRequest(t, http.MethodPatch, "/api/kb/sources/"+itoa(created.ID), env.tokenA, map[string]bool{"enabled": false})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	updated := decodeBody[SourceView](t, resp)
	assert.False(t, updated.Enabled)

	resp = env.jsonRequest(t, http.MethodDelete, "/api/kb/sources/"+itoa(created.ID), env.tokenA, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp = env.jsonRequest(t, http.MethodGet, "/api/kb/sources", env.tokenA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	after := decodeBody[struct {
		Sources []SourceView `json:"sources"`
	}](t, resp)
	assert.Empty(t, after.Sources)
}

func TestHandlerSourceRefusalHidesPaths(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	env := setupHandler(t, testConfig(base), &countingEmbedder{})

	resp := env.jsonRequest(t, http.MethodPost, "/api/kb/sources", env.tokenA, map[string]string{
		"name": "mine", "root_path": outside,
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	raw := bodyString(t, resp)
	assert.NotContains(t, raw, outside)
	assert.NotContains(t, raw, base)
}

func TestHandlerRequiresAuthentication(t *testing.T) {
	env := setupHandler(t, testConfig(""), &countingEmbedder{})
	resp := env.jsonRequest(t, http.MethodGet, "/api/kb/sources", "", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp = env.jsonRequest(t, http.MethodPost, "/api/kb/index", "", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandlerMultipartUploadAndCrossUserIsolation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	env := setupHandler(t, testConfig(""), &countingEmbedder{})
	content := []byte("# Notes\n\nuploaded body\n")
	body, contentType := multipartUpload(t, "notes.md", content)

	resp := env.request(t, http.MethodPost, "/api/kb/documents", env.tokenA, body, contentType)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeBody[DocumentView](t, resp)
	assert.Equal(t, OriginInline, created.Origin)
	assert.Equal(t, StatusIndexed, created.Status)
	require.NotNil(t, created.Content)
	assert.Equal(t, string(content), *created.Content)

	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, entries)

	resp = env.jsonRequest(t, http.MethodGet, "/api/kb/documents/"+itoa(created.ID), env.tokenA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	fetched := decodeBody[DocumentView](t, resp)
	require.NotNil(t, fetched.Content)
	assert.Equal(t, string(content), *fetched.Content)

	resp = env.jsonRequest(t, http.MethodGet, "/api/kb/documents/"+itoa(created.ID), env.tokenB, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandlerRefusesBinaryUpload(t *testing.T) {
	env := setupHandler(t, testConfig(""), &countingEmbedder{})
	body, contentType := multipartUpload(t, "image.png", []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01})

	resp := env.request(t, http.MethodPost, "/api/kb/documents", env.tokenA, body, contentType)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	raw := bodyString(t, resp)
	assert.Contains(t, raw, "text")
	assert.Empty(t, env.store.documents)
}

func TestHandlerCreatesJSONDocument(t *testing.T) {
	env := setupHandler(t, testConfig(""), &countingEmbedder{})
	resp := env.jsonRequest(t, http.MethodPost, "/api/kb/documents", env.tokenA, map[string]string{
		"name": "typed", "content": "# Typed\n\ntyped body\n",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeBody[DocumentView](t, resp)
	assert.Equal(t, OriginInline, created.Origin)
	assert.Equal(t, "typed.md", created.RelPath)

	resp = env.jsonRequest(t, http.MethodGet, "/api/kb/documents?origin=inline&status=indexed", env.tokenA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := decodeBody[struct {
		Documents []DocumentView `json:"documents"`
	}](t, resp)
	require.Len(t, list.Documents, 1)
	assert.Equal(t, created.ID, list.Documents[0].ID)
}

type blockingEmbedder struct{}

func (blockingEmbedder) EmbedStrings(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingEmbedder) Model() string { return "blocking" }

func TestHandlerUploadTimeoutLeavesDocumentPending(t *testing.T) {
	previous := inlineIndexLimit
	inlineIndexLimit = 50 * time.Millisecond
	t.Cleanup(func() { inlineIndexLimit = previous })

	env := setupHandler(t, testConfig(""), blockingEmbedder{})
	resp := env.jsonRequest(t, http.MethodPost, "/api/kb/documents", env.tokenA, map[string]string{
		"name": "slow", "content": "# Slow\n\nslow body\n",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeBody[DocumentView](t, resp)
	assert.Equal(t, StatusPending, created.Status)
}

func TestHandlerUpdateSourceRequiresEnabled(t *testing.T) {
	env := setupHandler(t, testConfig(""), &countingEmbedder{})
	src, err := env.store.UpsertUserDirSource(context.Background(), 1, "mine", t.TempDir())
	require.NoError(t, err)

	_, err = NewHandler(env.service).UpdateSource(
		reqctx.WithUserID(context.Background(), 1),
		&updateSourceInput{ID: src.ID},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid knowledge source update")
}

func TestHandlerUpdateInlineDocumentKeepsIdentity(t *testing.T) {
	env := setupHandler(t, testConfig(""), &countingEmbedder{})
	resp := env.jsonRequest(t, http.MethodPost, "/api/kb/documents", env.tokenA, map[string]string{
		"name": "notes", "content": "# Notes\n\nfirst body\n",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeBody[DocumentView](t, resp)

	resp = env.jsonRequest(t, http.MethodPut, "/api/kb/documents/"+itoa(created.ID), env.tokenA, map[string]string{
		"content": "# Notes\n\nsecond body\n",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	updated := decodeBody[DocumentView](t, resp)
	assert.Equal(t, created.ID, updated.ID)
	require.NotNil(t, updated.Content)
	assert.Equal(t, "# Notes\n\nsecond body\n", *updated.Content)

	resp = env.jsonRequest(t, http.MethodPut, "/api/kb/documents/"+itoa(created.ID), env.tokenB, map[string]string{"content": "stolen"})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandlerRefusesFileBackedEditAndDelete(t *testing.T) {
	base := t.TempDir()
	env := setupHandler(t, testConfig(base), &countingEmbedder{})
	src, err := env.store.UpsertUserDirSource(context.Background(), 1, "mine", base)
	require.NoError(t, err)
	doc, err := env.store.UpsertFileDocument(context.Background(), Document{
		SourceID: src.ID, Scope: ScopeUser, OwnerID: 1, Origin: OriginFile,
		RelPath: "disk.md", Title: "disk", DocType: DocTypeMarkdown,
		ContentHash: hashBytes([]byte("# Disk\n\nfile body\n")),
	})
	require.NoError(t, err)

	resp := env.jsonRequest(t, http.MethodPut, "/api/kb/documents/"+itoa(doc.ID), env.tokenA, map[string]string{"content": "new"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	raw := bodyString(t, resp)
	assert.NotContains(t, raw, base)
	assert.Equal(t, OriginFile, env.store.documents[doc.ID].Origin)

	resp = env.jsonRequest(t, http.MethodDelete, "/api/kb/documents/"+itoa(doc.ID), env.tokenA, nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	_, ok := env.store.documents[doc.ID]
	assert.True(t, ok)
}

func TestHandlerDeleteInlineDocumentRemovesChunks(t *testing.T) {
	env := setupHandler(t, testConfig(""), &countingEmbedder{})
	resp := env.jsonRequest(t, http.MethodPost, "/api/kb/documents", env.tokenA, map[string]string{
		"name": "gone", "content": "# Gone\n\nbody\n",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	created := decodeBody[DocumentView](t, resp)
	require.NotEmpty(t, env.store.chunksForDoc(created.ID))

	resp = env.jsonRequest(t, http.MethodDelete, "/api/kb/documents/"+itoa(created.ID), env.tokenA, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, env.store.chunksForDoc(created.ID))

	resp = env.jsonRequest(t, http.MethodGet, "/api/kb/documents/"+itoa(created.ID), env.tokenA, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandlerIndexReportsVisibleSources(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "alice")
	writeFile(t, filepath.Join(root, "note.md"), "# Note\n\nbody\n")
	env := setupHandler(t, testConfig(base), &countingEmbedder{})
	_, err := env.store.UpsertUserDirSource(context.Background(), 1, "mine", root)
	require.NoError(t, err)

	resp := env.jsonRequest(t, http.MethodPost, "/api/kb/index", env.tokenA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	report := decodeBody[IndexReportView](t, resp)
	require.Len(t, report.Sources, 1)
	assert.Equal(t, 1, report.Sources[0].Documents)
	assert.Greater(t, report.Sources[0].Chunks, 0)
	assert.NotEmpty(t, env.store.documents)
}

func TestHandlerSettings(t *testing.T) {
	base := t.TempDir()
	env := setupHandler(t, testConfig(base), &countingEmbedder{model: "model-q"})
	resp := env.jsonRequest(t, http.MethodGet, "/api/kb/settings", env.tokenA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	settings := decodeBody[Settings](t, resp)
	assert.True(t, settings.Enabled)
	assert.Equal(t, base, settings.PrivateBase)
	assert.Equal(t, "model-q", settings.EmbeddingModel)
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestOutlineHandlerListsVisibleCorpus(t *testing.T) {
	store := newFakeStore()
	shared, err := store.UpsertSharedSource(context.Background(), "shared", t.TempDir())
	require.NoError(t, err)
	doc, err := store.UpsertFileDocument(context.Background(), Document{
		SourceID: shared.ID, Scope: ScopeShared, Origin: OriginFile,
		RelPath: "B-Cook/a.md", Title: "a", DocType: DocTypeMarkdown,
		ContentHash: hashBytes([]byte("a")),
	})
	require.NoError(t, err)
	_, err = store.InsertChunk(context.Background(), Chunk{
		DocumentID: doc.ID, Scope: ScopeShared, SourceID: shared.ID,
		Generation: 1, Content: "secret body", EmbedText: "e", ContentHash: hashBytes([]byte("secret body")),
	})
	require.NoError(t, err)
	service := newTestService(store, &countingEmbedder{}, testConfig(""))
	handler := OutlineHandler(service)

	req := httptest.NewRequest(http.MethodGet, "/internal/rag/outline", nil)
	req = req.WithContext(reqctx.WithUserID(req.Context(), 1))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var outline Outline
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &outline))
	require.Len(t, outline.Sources, 1)
	assert.Equal(t, int64(1), outline.Sources[0].Documents)
	assert.Equal(t, int64(1), outline.Sources[0].Chunks)
	assert.Equal(t, []string{"B-Cook"}, outline.Sources[0].Directories)
	assert.NotContains(t, rr.Body.String(), "secret body")

	unauth := httptest.NewRequest(http.MethodGet, "/internal/rag/outline", nil)
	unauthRR := httptest.NewRecorder()
	handler.ServeHTTP(unauthRR, unauth)
	assert.Equal(t, http.StatusUnauthorized, unauthRR.Code)
}

func TestOpenAPIDoesNotAdvertiseInternalRoutes(t *testing.T) {
	store := newFakeStore()
	service := newTestService(store, &countingEmbedder{}, testConfig(""))
	tokens, err := auth.NewTokenService([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute)
	require.NoError(t, err)

	router := chi.NewMux()
	hapi := humachi.New(router, api.HumaConfig())
	RegisterKBOpsTest(hapi, NewHandler(service), tokens, false)
	server := httptest.NewServer(router)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/swagger/doc.json")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "/api/kb/sources")
	assert.NotContains(t, string(raw), "/internal/rag/outline")
	assert.NotContains(t, string(raw), "/internal/rag/retrieve")
}
