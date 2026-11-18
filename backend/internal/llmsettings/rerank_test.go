package llmsettings

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/platform/config"
)

const (
	localRerankURL  = "http://localhost:8002"
	remoteRerankURL = "https://rerank.example"
)

func TestService_RerankGroupRoundTrips(t *testing.T) {
	svc, _, _ := newTestService(t)
	in := validCreateInput()
	in.RerankBaseURL = "https://api.jina.ai/v1"
	in.RerankAPIKey = "rerankkey0000"
	in.RerankModel = "jina-reranker-v2-base-multilingual"

	require.NoError(t, svc.Save(context.Background(), 7, in))
	got, err := svc.Get(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, "https://api.jina.ai/v1", got.RerankBaseURL)
	assert.Equal(t, "jina-reranker-v2-base-multilingual", got.RerankModel)
	assert.Equal(t, "rerankkey0000", got.RerankAPIKey)
}

func TestService_RerankLoopbackNeedsNoKey(t *testing.T) {
	svc, _, _ := newTestService(t)
	in := validCreateInput()
	in.RerankBaseURL = localRerankURL
	in.RerankModel = "Qwen/Qwen3-Reranker-0.6B"

	require.NoError(t, svc.Save(context.Background(), 7, in))
	got, err := svc.Get(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, "", got.RerankAPIKey)
}

func TestValidate_RerankGroupRules(t *testing.T) {
	base := validCreateInput()
	base.RerankBaseURL = ""
	base.RerankAPIKey = ""
	base.RerankModel = ""

	cases := []struct {
		name    string
		mutate  func(*UpdateInput)
		wantErr string
	}{
		{"model without base url", func(in *UpdateInput) { in.RerankModel = "m" }, "rerank_base_url"},
		{"key without base url", func(in *UpdateInput) { in.RerankAPIKey = "k" }, "rerank_base_url"},
		{"base url without model", func(in *UpdateInput) { in.RerankBaseURL = remoteRerankURL }, "rerank_model"},
		{"remote without key", func(in *UpdateInput) {
			in.RerankBaseURL = remoteRerankURL
			in.RerankModel = "m"
		}, "rerank_api_key"},
		{"placeholder key", func(in *UpdateInput) {
			in.RerankBaseURL = remoteRerankURL
			in.RerankModel = "m"
			in.RerankAPIKey = "your-key-placeholder"
		}, "rerank_api_key"},
		{"non-http scheme", func(in *UpdateInput) {
			in.RerankBaseURL = "ftp://rerank.example"
			in.RerankModel = "m"
			in.RerankAPIKey = "k"
		}, "rerank_base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			err := ValidateUpdateWithPolicy(&in, nil, true)
			require.Error(t, err)
			var fieldErr *InvalidFieldError
			require.ErrorAs(t, err, &fieldErr)
			assert.Equal(t, tc.wantErr, fieldErr.Field)
		})
	}

	ok := base
	ok.RerankBaseURL = localRerankURL
	ok.RerankModel = "m"
	assert.NoError(t, ValidateUpdateWithPolicy(&ok, nil, true))
}

func TestService_RerankOnlyConfigurationIsAccepted(t *testing.T) {
	svc, _, _ := newTestService(t)
	in := UpdateInput{
		RerankBaseURL: localRerankURL,
		RerankModel:   "Qwen/Qwen3-Reranker-0.6B",
	}
	require.NoError(t, svc.Save(context.Background(), 8, in))
	got, err := svc.Get(context.Background(), 8)
	require.NoError(t, err)
	assert.Equal(t, "", got.LLMBaseURL)
	assert.Equal(t, localRerankURL, got.RerankBaseURL)
}

func TestResolver_RerankSlotPerUserAndCached(t *testing.T) {
	r, svc := newResolverFixture(t)
	ctx := context.Background()

	in7 := validCreateInput()
	in7.RerankBaseURL = localRerankURL
	in7.RerankModel = "model-seven"
	in8 := validCreateInput()
	in8.RerankBaseURL = "http://localhost:8003"
	in8.RerankModel = "model-eight"
	require.NoError(t, svc.Save(ctx, 7, in7))
	require.NoError(t, svc.Save(ctx, 8, in8))
	require.NoError(t, svc.Save(ctx, 9, validCreateInput()))

	stack7, err := r.Resolve(ctx, 7)
	require.NoError(t, err)
	stack8, err := r.Resolve(ctx, 8)
	require.NoError(t, err)
	stack9, err := r.Resolve(ctx, 9)
	require.NoError(t, err)

	require.NotNil(t, stack7.Rerank)
	require.NotNil(t, stack8.Rerank)
	assert.Nil(t, stack9.Rerank, "a user without a rerank group resolves to none")
	assert.NotEqual(t, stack7.Rerank.Key(), stack8.Rerank.Key())

	again, err := r.Resolve(ctx, 7)
	require.NoError(t, err)
	assert.Same(t, stack7.Rerank, again.Rerank, "the rerank client is cached per slot")
}

func newRerankProbeServer(t *testing.T, status int, body string) (*httptest.Server, *[]byte, *string) {
	t.Helper()
	var gotBody []byte
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth = req.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody, &auth
}

func newProbeResolver(t *testing.T) *Resolver {
	t.Helper()
	svc, _, _ := newTestService(t)
	base := &config.Config{LLMConfigProbeTimeout: "2s", BYOKAllowPrivateURL: true}
	return NewResolver(svc, base, noop.NewTracerProvider())
}

func TestValidate_RerankProbeOK(t *testing.T) {
	srv, body, auth := newRerankProbeServer(t, http.StatusOK, `{"results":[{"index":0,"relevance_score":0.9}]}`)
	r := newProbeResolver(t)

	res, err := r.ValidateValues(context.Background(), 1, UpdateInput{
		RerankBaseURL: srv.URL,
		RerankAPIKey:  "secret-rerank-key",
		RerankModel:   "Qwen/Qwen3-Reranker-0.6B",
	}, []string{"rerank"})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Rerank)
	assert.True(t, res.Slots.Rerank.Probed)
	assert.Equal(t, ProbeOK, res.Slots.Rerank.Status)
	assert.Equal(t, http.StatusOK, res.Slots.Rerank.HTTPStatus)
	assert.Equal(t, "Bearer secret-rerank-key", *auth)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(*body, &payload))
	assert.Equal(t, "Qwen/Qwen3-Reranker-0.6B", payload["model"])
	assert.NotEmpty(t, payload["query"])
	docs, ok := payload["documents"].([]any)
	require.True(t, ok)
	assert.Len(t, docs, 1)
	assert.Equal(t, float64(1), payload["top_n"])

	require.NotNil(t, res.Slots.Rerank.Request)
	assert.Equal(t, http.MethodPost, res.Slots.Rerank.Request.Method)
	assert.Contains(t, res.Slots.Rerank.Request.URL, "/rerank")
	assert.NotContains(t, res.Slots.Rerank.Request.Headers["Authorization"], "secret-rerank-key")
}

func TestValidate_RerankProbeStatusMatrix(t *testing.T) {
	cases := []struct {
		status int
		want   string
		body   string
	}{
		{http.StatusUnauthorized, ProbeAuthFailed, ``},
		{http.StatusTooManyRequests, ProbeRateLimited, ``},
		{http.StatusNotFound, ProbeInvalidModel, ``},
		{http.StatusBadRequest, ProbeInvalidConfig, ``},
		{http.StatusInternalServerError, ProbeUnreachable, ``},
		{http.StatusOK, ProbeInvalidConfig, `{"results":[]}`},
		{http.StatusOK, ProbeInvalidConfig, `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			srv, _, _ := newRerankProbeServer(t, tc.status, tc.body)
			r := newProbeResolver(t)
			res, err := r.ValidateValues(context.Background(), 1, UpdateInput{
				RerankBaseURL: srv.URL,
				RerankModel:   "m",
			}, []string{"rerank"})
			require.NoError(t, err)
			require.NotNil(t, res.Slots.Rerank)
			assert.Equal(t, tc.want, res.Slots.Rerank.Status)
		})
	}
}

func TestValidate_RerankProbeUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	r := newProbeResolver(t)
	res, err := r.ValidateValues(context.Background(), 1, UpdateInput{
		RerankBaseURL: url,
		RerankModel:   "m",
	}, []string{"rerank"})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Rerank)
	assert.Equal(t, ProbeUnreachable, res.Slots.Rerank.Status)
}

func TestValidate_RerankProbeSlowResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":0.1}]}`)
	}))
	t.Cleanup(srv.Close)

	svc, _, _ := newTestService(t)
	base := &config.Config{LLMConfigProbeTimeout: "50ms", BYOKAllowPrivateURL: true}
	r := NewResolver(svc, base, noop.NewTracerProvider())

	res, err := r.ValidateValues(context.Background(), 1, UpdateInput{
		RerankBaseURL: srv.URL,
		RerankModel:   "m",
	}, []string{"rerank"})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Rerank)
	assert.Equal(t, ProbeSlowResponse, res.Slots.Rerank.Status)
	assert.Equal(t, http.StatusOK, res.Slots.Rerank.HTTPStatus)
}

func TestValidate_RerankProbeRemoteWithoutKeyIsInvalidConfig(t *testing.T) {
	r := newProbeResolver(t)
	res, err := r.ValidateValues(context.Background(), 1, UpdateInput{
		RerankBaseURL: "https://rerank.example/v1",
		RerankModel:   "m",
	}, []string{"rerank"})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Rerank)
	assert.Equal(t, ProbeInvalidConfig, res.Slots.Rerank.Status)
	assert.True(t, strings.Contains(res.Slots.Rerank.Detail, "api key"))
}
