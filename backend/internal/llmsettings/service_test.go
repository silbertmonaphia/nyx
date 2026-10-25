package llmsettings

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/platform/crypto"
)

//nolint:gosec // G101: test fixture, not a real credential.
const testEncryptionSecret = "nyx-llmsettings-service-test-secret-0123456789abcdef"

const (
	testFallbackURL = "http://fallback:8000/v1"
	testFallbackKey = "sk-fallback-key-0009"
)

type stubRepo struct {
	rows  map[int]*EncryptedSettings
	calls []string
}

func newStubRepo() *stubRepo {
	return &stubRepo{rows: map[int]*EncryptedSettings{}}
}

func (r *stubRepo) Get(_ context.Context, userID int) (*EncryptedSettings, error) {
	r.calls = append(r.calls, "get")
	row, ok := r.rows[userID]
	if !ok {
		return nil, ErrNotFound
	}
	return row, nil
}

func (r *stubRepo) Upsert(_ context.Context, s *EncryptedSettings) (*EncryptedSettings, error) {
	r.calls = append(r.calls, "upsert")
	r.rows[s.UserID] = s
	return s, nil
}

func (r *stubRepo) Delete(_ context.Context, userID int) (int64, error) {
	r.calls = append(r.calls, "delete")
	if _, ok := r.rows[userID]; !ok {
		return 0, nil
	}
	delete(r.rows, userID)
	return 1, nil
}

func newTestService(t *testing.T) (*Service, *stubRepo, *crypto.Cipher) {
	t.Helper()
	cipher, err := crypto.NewCipher(testEncryptionSecret)
	require.NoError(t, err)
	repo := newStubRepo()
	return NewService(repo, cipher), repo, cipher
}

func validCreateInput() UpdateInput {
	return UpdateInput{
		LLMBaseURL:       "http://localhost:8000/v1",
		LLMAPIKey:        "sk-real-user-key-0001",
		LLMModel:         "Qwen/Qwen3-4B-Instruct-2507-FP8",
		EmbeddingBaseURL: "http://localhost:8001/v1",
		EmbeddingAPIKey:  "sk-real-embed-key-0002",
		EmbeddingModel:   "Qwen/Qwen3-Embedding-0.6B",
		TavilyBaseURL:    "https://api.tavily.com/search",
		TavilyAPIKey:     "tvly-real-key-0003",
	}
}

func seedEncrypted(t *testing.T, svc *Service, repo *stubRepo, userID int) *EncryptedSettings {
	t.Helper()
	require.NoError(t, svc.Save(context.Background(), userID, validCreateInput()))
	return repo.rows[userID]
}

func TestService_Save_CreateEncryptsKeys(t *testing.T) {
	svc, repo, cipher := newTestService(t)

	require.NoError(t, svc.Save(context.Background(), 1, validCreateInput()))

	row := repo.rows[1]
	require.NotNil(t, row)
	assert.Equal(t, "http://localhost:8000/v1", row.LLMBaseURL)
	assert.NotEqual(t, "sk-real-user-key-0001", row.LLMAPIKeyEnc, "the key must be stored as ciphertext")
	assert.True(t, strings.HasPrefix(row.LLMAPIKeyEnc, "v1:"), "ciphertext must carry the v1: prefix")

	plain, err := cipher.Decrypt(row.LLMAPIKeyEnc)
	require.NoError(t, err)
	assert.Equal(t, "sk-real-user-key-0001", plain)
}

func TestService_Save_UpdateBlankKeyKeepsStoredCiphertext(t *testing.T) {
	svc, repo, _ := newTestService(t)
	stored := seedEncrypted(t, svc, repo, 1)
	originalEnc := stored.LLMAPIKeyEnc

	in := validCreateInput()
	in.LLMAPIKey = ""
	in.EmbeddingAPIKey = ""
	in.TavilyAPIKey = ""
	in.LLMBaseURL = "http://moved:8000/v1"
	require.NoError(t, svc.Save(context.Background(), 1, in))

	row := repo.rows[1]
	assert.Equal(t, originalEnc, row.LLMAPIKeyEnc,
		"a blank key field must pass the stored ciphertext through untouched")
	assert.Equal(t, stored.EmbeddingAPIKeyEnc, row.EmbeddingAPIKeyEnc)
	assert.Equal(t, stored.TavilyAPIKeyEnc, row.TavilyAPIKeyEnc)
	assert.Equal(t, "http://moved:8000/v1", row.LLMBaseURL, "non-key fields update")
}

func TestService_Get_DecryptsKeys(t *testing.T) {
	svc, repo, _ := newTestService(t)
	seedEncrypted(t, svc, repo, 2)

	got, err := svc.Get(context.Background(), 2)
	require.NoError(t, err)
	assert.Equal(t, "sk-real-user-key-0001", got.LLMAPIKey)
	assert.Equal(t, "sk-real-embed-key-0002", got.EmbeddingAPIKey)
	assert.Equal(t, "tvly-real-key-0003", got.TavilyAPIKey)
	assert.Equal(t, "http://localhost:8000/v1", got.LLMBaseURL)
}

func TestService_Get_NotFound(t *testing.T) {
	svc, _, _ := newTestService(t)

	_, err := svc.Get(context.Background(), 99)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestService_Delete_Idempotent(t *testing.T) {
	svc, repo, _ := newTestService(t)
	seedEncrypted(t, svc, repo, 3)

	n, err := svc.Delete(context.Background(), 3)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	n, err = svc.Delete(context.Background(), 3)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "second delete affects zero rows — idempotent")
}

func TestMaskKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abcd", "…"},
		{"abcde", "…bcde"},
		{"sk-live-abcdefgh", "…efgh"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, MaskKey(c.in), "MaskKey(%q)", c.in)
	}

	full := "sk-super-secret-value"
	masked := MaskKey(full)
	assert.False(t, strings.Contains(masked, full), "the masked preview must never contain the full key")
	assert.LessOrEqual(t, len([]rune(masked)), 5, "the preview carries at most the tail 4 characters plus the ellipsis")
}

func TestValidateUserURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"http localhost", "http://localhost:8000/v1", false},
		{"https public", "https://api.openai.com/v1", false},
		{"private RFC1918 allowed by design", "http://192.168.1.10:8000/v1", false},
		{"docker hostname allowed", "http://vllm:8000/v1", false},
		{"ftp scheme rejected", "ftp://files.example.com", true},
		{"file scheme rejected", "file:///etc/passwd", true},
		{"empty scheme rejected", "localhost:8000/v1", true},
		{"missing host rejected", "http://", true},
		{"userinfo rejected", "http://user:pw@host.example.com/v1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateUserURL(c.raw, "llm_base_url")
			if c.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidConfig)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

const (
	testFallbackModel     = "fallback-model"
	testStoredFallbackEnc = "v1:stored-fallback"
	testRemoteLLMBaseURL  = "https://api.example.com/v1"
)

func TestValidateUpdate(t *testing.T) {
	existing := &EncryptedSettings{
		UserID:             1,
		LLMBaseURL:         "http://old:8000/v1",
		LLMAPIKeyEnc:       "v1:stored-llm",
		LLMModel:           "old-model",
		EmbeddingBaseURL:   "http://old:8001/v1",
		EmbeddingAPIKeyEnc: "v1:stored-embed",
		EmbeddingModel:     "old-embed",
		TavilyBaseURL:      "https://old-tavily",
		TavilyAPIKeyEnc:    "v1:stored-tavily",
	}

	t.Run("create with all eight required fields passes", func(t *testing.T) {
		in := validCreateInput()
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("create with a blank required key is rejected per field", func(t *testing.T) {
		for _, field := range []struct {
			mutate func(*UpdateInput)
			want   string
		}{
			{func(in *UpdateInput) { in.LLMBaseURL = testRemoteLLMBaseURL; in.LLMAPIKey = "" }, "llm_api_key"},
			{func(in *UpdateInput) { in.EmbeddingBaseURL = "https://embed.example.com/v1"; in.EmbeddingAPIKey = "" }, "embedding_api_key"},
		} {
			in := validCreateInput()
			field.mutate(&in)
			err := ValidateUpdate(&in, nil)
			require.Error(t, err)
			var fe *InvalidFieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, field.want, fe.Field, "the failure must name the missing field")
		}
	})

	t.Run("update with blank keys passes keep-through", func(t *testing.T) {
		in := validCreateInput()
		in.LLMAPIKey = ""
		in.EmbeddingAPIKey = ""
		in.TavilyAPIKey = ""
		assert.NoError(t, ValidateUpdate(&in, existing))
	})

	t.Run("update with all-empty fallback clears a stored group", func(t *testing.T) {
		stored := *existing
		stored.FallbackBaseURL = "http://fallback:8000/v1"
		stored.FallbackAPIKeyEnc = testStoredFallbackEnc
		stored.FallbackModel = testFallbackModel

		in := validCreateInput()
		in.FallbackBaseURL = ""
		in.FallbackAPIKey = ""
		in.FallbackModel = ""
		assert.NoError(t, ValidateUpdate(&in, &stored))
	})

	t.Run("update keeping a stored fallback key with new endpoint passes", func(t *testing.T) {
		stored := *existing
		stored.FallbackBaseURL = "http://fallback:8000/v1"
		stored.FallbackAPIKeyEnc = testStoredFallbackEnc
		stored.FallbackModel = testFallbackModel

		in := validCreateInput()
		in.FallbackBaseURL = "http://fallback2:8000/v1"
		in.FallbackAPIKey = ""
		in.FallbackModel = "fallback-model-2"
		assert.NoError(t, ValidateUpdate(&in, &stored))
	})

	t.Run("required non-key fields are enforced on update too", func(t *testing.T) {
		for _, field := range []struct {
			mutate func(*UpdateInput)
			want   string
		}{
			{func(in *UpdateInput) { in.LLMBaseURL = "" }, "llm_base_url"},
			{func(in *UpdateInput) { in.LLMModel = "" }, "llm_model"},
			{func(in *UpdateInput) { in.EmbeddingBaseURL = "" }, "embedding_base_url"},
			{func(in *UpdateInput) { in.EmbeddingModel = "" }, "embedding_model"},
		} {
			in := validCreateInput()
			field.mutate(&in)
			err := ValidateUpdate(&in, existing)
			require.Error(t, err)
			var fe *InvalidFieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, field.want, fe.Field)
		}
	})

	t.Run("provided placeholder keys are rejected", func(t *testing.T) {
		for _, field := range []struct {
			mutate func(*UpdateInput)
			want   string
		}{
			{func(in *UpdateInput) { in.LLMAPIKey = "changeme" }, "llm_api_key"},
			{func(in *UpdateInput) { in.EmbeddingAPIKey = "your-key" }, "embedding_api_key"},
			{func(in *UpdateInput) { in.TavilyAPIKey = "sk-xxx" }, "tavily_api_key"},
		} {
			in := validCreateInput()
			field.mutate(&in)
			err := ValidateUpdate(&in, existing)
			require.Error(t, err)
			var fe *InvalidFieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, field.want, fe.Field)
		}
	})

	t.Run("bad scheme names the field", func(t *testing.T) {
		in := validCreateInput()
		in.EmbeddingBaseURL = "gopher://embed:8001"
		err := ValidateUpdate(&in, nil)
		require.Error(t, err)
		var fe *InvalidFieldError
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, "embedding_base_url", fe.Field)
	})

	t.Run("fallback group passes complete", func(t *testing.T) {
		in := validCreateInput()
		in.FallbackBaseURL = testFallbackURL
		in.FallbackAPIKey = testFallbackKey
		in.FallbackModel = "Qwen/Qwen2.5-3B-Instruct"
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("fallback group passes empty", func(t *testing.T) {
		in := validCreateInput()
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("fallback group rejects each partial combination", func(t *testing.T) {
		partials := []func(*UpdateInput){
			func(in *UpdateInput) { in.FallbackBaseURL = testFallbackURL },
			func(in *UpdateInput) { in.FallbackAPIKey = testFallbackKey },
			func(in *UpdateInput) { in.FallbackModel = "m" },
			func(in *UpdateInput) {
				in.FallbackBaseURL = testFallbackURL
				in.FallbackAPIKey = testFallbackKey
			},
			func(in *UpdateInput) {
				in.FallbackBaseURL = testFallbackURL
				in.FallbackModel = "m"
			},
			func(in *UpdateInput) {
				in.FallbackAPIKey = testFallbackKey
				in.FallbackModel = "m"
			},
		}
		for i, partial := range partials {
			in := validCreateInput()
			partial(&in)
			err := ValidateUpdate(&in, nil)
			require.Error(t, err, "partial combination %d must be rejected", i)
			var fe *InvalidFieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, "fallback", fe.Field, "partial combination %d", i)
		}
	})

	t.Run("fallback blank key on update keeps the stored ciphertext", func(t *testing.T) {
		withFallback := &EncryptedSettings{}
		*withFallback = *existing
		withFallback.FallbackBaseURL = "http://old-fallback:8000/v1"
		withFallback.FallbackAPIKeyEnc = "v1:stored-fallback"
		withFallback.FallbackModel = "old-fallback-model"

		in := validCreateInput()
		in.FallbackBaseURL = "http://new-fallback:8000/v1"
		in.FallbackModel = "new-fallback-model"
		assert.NoError(t, ValidateUpdate(&in, withFallback),
			"a blank fallback key counts as present when a stored ciphertext exists to keep")
	})

	t.Run("fallback partial with no stored key on update is rejected", func(t *testing.T) {
		in := validCreateInput()
		in.FallbackBaseURL = "http://new-fallback:8000/v1"
		in.FallbackModel = "new-fallback-model"
		err := ValidateUpdate(&in, existing)
		require.Error(t, err, "blank fallback key with no stored ciphertext is a partial group")
	})

	t.Run("create without embedding is accepted", func(t *testing.T) {
		in := validCreateInput()
		in.EmbeddingBaseURL = ""
		in.EmbeddingAPIKey = ""
		in.EmbeddingModel = ""
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("partial embedding group is rejected", func(t *testing.T) {
		for _, field := range []struct {
			mutate func(*UpdateInput)
			want   string
		}{
			{func(in *UpdateInput) { in.EmbeddingModel = "" }, "embedding_model"},
			{func(in *UpdateInput) { in.EmbeddingBaseURL = "" }, "embedding_base_url"},
		} {
			in := validCreateInput()
			field.mutate(&in)
			err := ValidateUpdate(&in, nil)
			require.Error(t, err)
			var fe *InvalidFieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, field.want, fe.Field)
		}
	})

	t.Run("remote embedding without a key is rejected", func(t *testing.T) {
		in := validCreateInput()
		in.EmbeddingBaseURL = "https://embed.example.com/v1"
		in.EmbeddingAPIKey = ""
		err := ValidateUpdate(&in, nil)
		require.Error(t, err)
		var fe *InvalidFieldError
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, "embedding_api_key", fe.Field)
	})

	t.Run("loopback embedding without a key is accepted", func(t *testing.T) {
		in := validCreateInput()
		in.EmbeddingBaseURL = "http://127.0.0.1:8001/v1"
		in.EmbeddingAPIKey = ""
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("create without Tavily is accepted", func(t *testing.T) {
		in := validCreateInput()
		in.TavilyBaseURL = ""
		in.TavilyAPIKey = ""
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("Tavily key without a base URL is rejected", func(t *testing.T) {
		in := validCreateInput()
		in.TavilyBaseURL = ""
		err := ValidateUpdate(&in, nil)
		require.Error(t, err)
		var fe *InvalidFieldError
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, "tavily_base_url", fe.Field)
	})

	t.Run("remote Tavily without a key is accepted for the keyless tier", func(t *testing.T) {
		in := validCreateInput()
		in.TavilyAPIKey = ""
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("invalid Tavily URL is rejected", func(t *testing.T) {
		in := validCreateInput()
		in.TavilyBaseURL = "ftp://api.tavily.com/search"
		err := ValidateUpdate(&in, nil)
		require.Error(t, err)
		var fe *InvalidFieldError
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, "tavily_base_url", fe.Field)
	})

	t.Run("create with a blank key on a loopback endpoint is accepted", func(t *testing.T) {
		in := validCreateInput()
		in.LLMAPIKey = ""
		in.EmbeddingAPIKey = ""
		in.TavilyBaseURL = "http://127.0.0.1:8899/search"
		in.TavilyAPIKey = ""
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("create with a blank key on a remote endpoint is rejected", func(t *testing.T) {
		in := validCreateInput()
		in.LLMBaseURL = testRemoteLLMBaseURL
		in.LLMAPIKey = ""
		err := ValidateUpdate(&in, nil)
		require.Error(t, err)
		var fe *InvalidFieldError
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, "llm_api_key", fe.Field)
	})

	t.Run("create accepts a loopback fallback without a key", func(t *testing.T) {
		in := validCreateInput()
		in.FallbackBaseURL = "http://localhost:9000/v1"
		in.FallbackModel = "fallback-model"
		in.FallbackAPIKey = ""
		assert.NoError(t, ValidateUpdate(&in, nil))
	})

	t.Run("errors chain to the registered ErrInvalidConfig sentinel", func(t *testing.T) {
		in := validCreateInput()
		in.LLMBaseURL = testRemoteLLMBaseURL
		in.LLMAPIKey = ""
		err := ValidateUpdate(&in, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidConfig)
	})
}

func TestService_Save_WiresRepositoryErrorsThrough(t *testing.T) {
	cipher, err := crypto.NewCipher(testEncryptionSecret)
	require.NoError(t, err)
	boom := errors.New("connection refused")
	repo := &failingRepo{err: boom}
	svc := NewService(repo, cipher)

	err = svc.Save(context.Background(), 1, validCreateInput())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidConfig, "a repository failure is not a validation failure")
}

type failingRepo struct{ err error }

func (r *failingRepo) Get(context.Context, int) (*EncryptedSettings, error) {
	return nil, ErrNotFound
}
func (r *failingRepo) Upsert(context.Context, *EncryptedSettings) (*EncryptedSettings, error) {
	return nil, r.err
}
func (r *failingRepo) Delete(context.Context, int) (int64, error) { return 0, r.err }
