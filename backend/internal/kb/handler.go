package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

const (
	maxUploadBytes  = 1 << 20
	safeBadBody     = "Invalid request body"
	safeBadUpload   = "Invalid upload"
	safeUnsupported = "Unsupported media type"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

type SourceView struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Scope          string     `json:"scope"`
	Kind           string     `json:"kind"`
	Enabled        bool       `json:"enabled"`
	LastScanAt     *time.Time `json:"last_scan_at"`
	LastError      string     `json:"last_error,omitempty"`
	DocumentCount  int64      `json:"document_count"`
	ChunkCount     int64      `json:"chunk_count"`
	PendingCount   int64      `json:"pending_count"`
	Directories    []string   `json:"directories"`
	EmbeddingModel string     `json:"embedding_model,omitempty"`
	IndexedModels  []string   `json:"indexed_models"`
}

type DocumentView struct {
	ID          int64      `json:"id"`
	SourceID    int64      `json:"source_id"`
	Origin      string     `json:"origin"`
	RelPath     string     `json:"rel_path"`
	Title       string     `json:"title"`
	DocType     string     `json:"doc_type"`
	ByteSize    int64      `json:"byte_size"`
	Status      string     `json:"status"`
	ErrorDetail string     `json:"error_detail,omitempty"`
	Generation  int32      `json:"generation"`
	Mtime       *time.Time `json:"mtime"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Content     *string    `json:"content,omitempty"`
	Headings    [][]string `json:"headings,omitempty"`
}

type ScanSkipView struct {
	RelPath string `json:"rel_path"`
	Reason  string `json:"reason"`
}

type SourceScanView struct {
	SourceID  int64          `json:"source_id"`
	Name      string         `json:"name"`
	Documents int            `json:"documents"`
	Chunks    int            `json:"chunks"`
	Skipped   []ScanSkipView `json:"skipped"`
	Error     string         `json:"error,omitempty"`
}

type IndexReportView struct {
	Sources []SourceScanView `json:"sources"`
}

func RegisterKBOps(api huma.API, h *Handler, tokens auth.TokenService) {
	RegisterKBOpsTest(api, h, tokens, true)
}

func RegisterKBOpsTest(api huma.API, h *Handler, tokens auth.TokenService, withAuth bool) {
	middlewares := huma.Middlewares{}
	if withAuth {
		middlewares = huma.Middlewares{middleware.NewHumaAuth(tokens)}
	}

	huma.Register(api, huma.Operation{
		OperationID: "list-kb-sources",
		Method:      http.MethodGet,
		Path:        "/api/kb/sources",
		Summary:     "List knowledge sources",
		Tags:        []string{"knowledge"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: middlewares,
	}, h.ListSources)

	huma.Register(api, huma.Operation{
		OperationID: "create-kb-source",
		Method:      http.MethodPost,
		Path:        "/api/kb/sources",
		Summary:     "Register a private knowledge directory",
		Tags:        []string{"knowledge"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: middlewares,
	}, h.CreateSource)

	huma.Register(api, huma.Operation{
		OperationID: "update-kb-source",
		Method:      http.MethodPatch,
		Path:        "/api/kb/sources/{id}",
		Summary:     "Enable or disable a knowledge source",
		Tags:        []string{"knowledge"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: middlewares,
	}, h.UpdateSource)

	huma.Register(api, huma.Operation{
		OperationID:   "delete-kb-source",
		Method:        http.MethodDelete,
		Path:          "/api/kb/sources/{id}",
		Summary:       "Remove a knowledge source",
		Tags:          []string{"knowledge"},
		Security:      []map[string][]string{{"BearerAuth": {}}},
		Middlewares:   middlewares,
		DefaultStatus: http.StatusNoContent,
	}, h.DeleteSource)

	huma.Register(api, huma.Operation{
		OperationID: "list-kb-documents",
		Method:      http.MethodGet,
		Path:        "/api/kb/documents",
		Summary:     "List knowledge documents",
		Tags:        []string{"knowledge"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: middlewares,
	}, h.ListDocuments)

	huma.Register(api, huma.Operation{
		OperationID: "get-kb-document",
		Method:      http.MethodGet,
		Path:        "/api/kb/documents/{id}",
		Summary:     "Get a knowledge document",
		Tags:        []string{"knowledge"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: middlewares,
	}, h.GetDocument)

	huma.Register(api, huma.Operation{
		OperationID: "update-kb-document",
		Method:      http.MethodPut,
		Path:        "/api/kb/documents/{id}",
		Summary:     "Update an inline knowledge document",
		Tags:        []string{"knowledge"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: middlewares,
	}, h.UpdateDocument)

	huma.Register(api, huma.Operation{
		OperationID:   "delete-kb-document",
		Method:        http.MethodDelete,
		Path:          "/api/kb/documents/{id}",
		Summary:       "Delete an inline knowledge document",
		Tags:          []string{"knowledge"},
		Security:      []map[string][]string{{"BearerAuth": {}}},
		Middlewares:   middlewares,
		DefaultStatus: http.StatusNoContent,
	}, h.DeleteDocument)

	huma.Register(api, huma.Operation{
		OperationID: "index-kb-sources",
		Method:      http.MethodPost,
		Path:        "/api/kb/index",
		Summary:     "Index the caller's visible knowledge sources",
		Tags:        []string{"knowledge"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: middlewares,
	}, h.Index)

	huma.Register(api, huma.Operation{
		OperationID: "get-kb-settings",
		Method:      http.MethodGet,
		Path:        "/api/kb/settings",
		Summary:     "Get knowledge base settings for the caller",
		Tags:        []string{"knowledge"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: middlewares,
	}, h.GetSettings)
}

func OutlineHandler(service *Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := reqctx.UserIDFromContext(r.Context())
		if userID == 0 {
			api.WriteError(w, r, http.StatusUnauthorized, "Authentication required", nil)
			return
		}
		outline, err := service.Outline(r.Context(), int64(userID))
		if err != nil {
			api.WriteError(w, r, http.StatusInternalServerError, "Failed to load knowledge outline",
				api.ClassifyAndLog(r.Context(), err, "Failed to load knowledge outline"))
			return
		}
		writeJSON(w, http.StatusOK, outline)
	})
}

func RegisterKBUploadRoute(router chi.Router, h *Handler, tokens auth.TokenService) {
	router.Group(func(r chi.Router) {
		r.Use(middleware.NewAuth(tokens))
		r.Post("/api/kb/documents", h.CreateDocumentHTTP)
	})
}

type listSourcesOutput struct {
	Body struct {
		Sources []SourceView `json:"sources"`
	}
}

type createSourceInput struct {
	Body struct {
		Name     string `json:"name" required:"true" maxLength:"200"`
		RootPath string `json:"root_path" required:"true" maxLength:"1024"`
	}
}

type sourceOutput struct {
	Body SourceView
}

type createSourceOutput struct {
	Status int `status:"201"`
	Body   SourceView
}

type sourcePathInput struct {
	ID int64 `path:"id" required:"true" minimum:"1"`
}

type updateSourceInput struct {
	ID   int64 `path:"id" required:"true" minimum:"1"`
	Body struct {
		Enabled *bool `json:"enabled" required:"true"`
	}
}

type listDocumentsInput struct {
	SourceID int64  `query:"source_id" required:"false" minimum:"0"`
	Origin   string `query:"origin" required:"false"`
	Status   string `query:"status" required:"false"`
	Limit    int    `query:"limit" required:"false" default:"50" minimum:"1" maximum:"100"`
	Offset   int    `query:"offset" required:"false" default:"0" minimum:"0"`
}

type listDocumentsOutput struct {
	Body struct {
		Documents []DocumentView `json:"documents"`
	}
}

type documentPathInput struct {
	ID int64 `path:"id" required:"true" minimum:"1"`
}

type documentOutput struct {
	Body DocumentView
}

type updateDocumentInput struct {
	ID   int64 `path:"id" required:"true" minimum:"1"`
	Body struct {
		Content string `json:"content" required:"true" maxLength:"1048576"`
	}
}

type deleteSourceOutput struct{}

type deleteDocumentOutput struct{}

type indexOutput struct {
	Body IndexReportView
}

type settingsOutput struct {
	Body Settings
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) ListSources(ctx context.Context, _ *struct{}) (*listSourcesOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	infos, err := h.service.ListSources(ctx, int64(userID))
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to list knowledge sources")
	}
	out := &listSourcesOutput{}
	out.Body.Sources = make([]SourceView, 0, len(infos))
	for _, info := range infos {
		out.Body.Sources = append(out.Body.Sources, sourceView(info))
	}
	return out, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) CreateSource(ctx context.Context, in *createSourceInput) (*createSourceOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	src, err := h.service.RegisterSource(ctx, int64(userID), in.Body.Name, in.Body.RootPath)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to register knowledge source")
	}
	info := SourceInfo{Source: src, EmbedModel: h.service.EmbeddingIdentity(ctx, int64(userID))}
	return &createSourceOutput{Status: http.StatusCreated, Body: sourceView(info)}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) UpdateSource(ctx context.Context, in *updateSourceInput) (*sourceOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	if in.Body.Enabled == nil {
		return nil, api.MapError(ctx, ErrInvalidSourceUpdate, "Failed to update knowledge source")
	}
	src, err := h.service.SetSourceEnabled(ctx, int64(userID), in.ID, *in.Body.Enabled)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to update knowledge source")
	}
	stats, err := h.service.SourceStats(ctx, src)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to update knowledge source")
	}
	info := SourceInfo{Source: src, Stats: stats, EmbedModel: h.service.EmbeddingIdentity(ctx, int64(userID))}
	return &sourceOutput{Body: sourceView(info)}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) DeleteSource(ctx context.Context, in *sourcePathInput) (*deleteSourceOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	if err := h.service.DeleteSource(ctx, int64(userID), in.ID); err != nil {
		return nil, api.MapError(ctx, err, "Failed to delete knowledge source")
	}
	return &deleteSourceOutput{}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) ListDocuments(ctx context.Context, in *listDocumentsInput) (*listDocumentsOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	var sourceID *int64
	if in.SourceID > 0 {
		sourceID = &in.SourceID
	}
	docs, err := h.service.ListDocuments(ctx, int64(userID), sourceID, in.Origin, in.Status, in.Limit, in.Offset)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to list knowledge documents")
	}
	out := &listDocumentsOutput{}
	out.Body.Documents = make([]DocumentView, 0, len(docs))
	for _, doc := range docs {
		out.Body.Documents = append(out.Body.Documents, documentView(doc, false))
	}
	return out, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) GetDocument(ctx context.Context, in *documentPathInput) (*documentOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	doc, err := h.service.GetDocument(ctx, int64(userID), in.ID)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to retrieve knowledge document")
	}
	headings, err := h.service.DocumentHeadings(ctx, int64(userID), in.ID)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to retrieve knowledge document")
	}
	view := documentView(doc, true)
	view.Headings = headings
	return &documentOutput{Body: view}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) UpdateDocument(ctx context.Context, in *updateDocumentInput) (*documentOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	doc, err := h.service.UpdateInlineDocument(ctx, int64(userID), in.ID, in.Body.Content)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to update knowledge document")
	}
	return &documentOutput{Body: documentView(doc, true)}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) DeleteDocument(ctx context.Context, in *documentPathInput) (*deleteDocumentOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	if err := h.service.DeleteInlineDocument(ctx, int64(userID), in.ID); err != nil {
		return nil, api.MapError(ctx, err, "Failed to delete knowledge document")
	}
	return &deleteDocumentOutput{}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) Index(ctx context.Context, _ *struct{}) (*indexOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	report, err := h.service.IndexVisible(ctx, int64(userID))
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to index knowledge sources")
	}
	out := &indexOutput{Body: IndexReportView{Sources: make([]SourceScanView, 0, len(report.Sources))}}
	for _, source := range report.Sources {
		out.Body.Sources = append(out.Body.Sources, SourceScanView{
			SourceID:  source.SourceID,
			Name:      source.Name,
			Documents: source.Result.Documents,
			Chunks:    source.Result.Chunks,
			Skipped:   skipViews(source.Result.Skipped),
			Error:     staticScanError(source.Err),
		})
	}
	return out, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) GetSettings(ctx context.Context, _ *struct{}) (*settingsOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	return &settingsOutput{Body: h.service.Settings(ctx, int64(userID))}, nil
}

type createDocumentJSONBody struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) CreateDocumentHTTP(w http.ResponseWriter, r *http.Request) {
	userID := reqctx.UserIDFromContext(r.Context())
	contentType := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(contentType, "multipart/form-data"):
		h.createDocumentMultipart(w, r, int64(userID))
	case strings.HasPrefix(contentType, "application/json"):
		h.createDocumentJSON(w, r, int64(userID))
	default:
		api.WriteError(w, r, http.StatusUnsupportedMediaType, safeUnsupported, nil)
	}
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) createDocumentJSON(w http.ResponseWriter, r *http.Request, userID int64) {
	var body createDocumentJSONBody
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxUploadBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		api.WriteError(w, r, http.StatusBadRequest, safeBadBody, api.ClassifyAndLog(r.Context(), err, safeBadBody))
		return
	}
	if !isTextContent([]byte(body.Content)) {
		writeMappedError(w, r, ErrNotText, "Invalid document body")
		return
	}
	doc, err := h.service.CreateInlineDocument(r.Context(), userID, body.Name, body.Content)
	if err != nil {
		writeMappedError(w, r, err, "Failed to create knowledge document")
		return
	}
	writeJSON(w, http.StatusCreated, documentView(doc, true))
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) createDocumentMultipart(w http.ResponseWriter, r *http.Request, userID int64) {
	reader, err := r.MultipartReader()
	if err != nil {
		api.WriteError(w, r, http.StatusBadRequest, safeBadUpload, api.ClassifyAndLog(r.Context(), err, safeBadUpload))
		return
	}
	var name string
	var content []byte
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			api.WriteError(w, r, http.StatusBadRequest, safeBadUpload, api.ClassifyAndLog(r.Context(), partErr, safeBadUpload))
			return
		}
		switch part.FormName() {
		case "file", "document":
			if name == "" {
				name = part.FileName()
			}
			raw, readErr := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
			_ = part.Close()
			if readErr != nil || len(raw) > maxUploadBytes {
				api.WriteError(w, r, http.StatusBadRequest, safeBadUpload, api.ClassifyAndLog(r.Context(), readErr, safeBadUpload))
				return
			}
			content = raw
		case "name":
			raw, _ := io.ReadAll(io.LimitReader(part, 256))
			_ = part.Close()
			name = strings.TrimSpace(string(raw))
		default:
			_ = part.Close()
		}
	}
	if len(content) == 0 {
		writeMappedError(w, r, ErrEmptyBody, "Document body is required")
		return
	}
	if !isTextContent(content) {
		writeMappedError(w, r, ErrNotText, "Only text documents can be uploaded")
		return
	}
	doc, err := h.service.CreateInlineDocument(r.Context(), userID, name, string(content))
	if err != nil {
		writeMappedError(w, r, err, "Failed to create knowledge document")
		return
	}
	writeJSON(w, http.StatusCreated, documentView(doc, true))
}

func writeMappedError(w http.ResponseWriter, r *http.Request, err error, safeDetail string) {
	resp := api.MapError(r.Context(), err, safeDetail)
	api.WriteError(w, r, resp.Code, resp.Message, resp.Details)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func isTextContent(raw []byte) bool {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return false
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return false
	}
	detected := http.DetectContentType(raw)
	return strings.HasPrefix(detected, "text/") || detected == "application/octet-stream"
}

func sourceView(info SourceInfo) SourceView {
	dirs := info.TopDirs
	if dirs == nil {
		dirs = []string{}
	}
	models := info.IndexedModels
	if models == nil {
		models = []string{}
	}
	return SourceView{
		ID:             info.ID,
		Name:           info.Name,
		Scope:          info.Scope,
		Kind:           info.Kind,
		Enabled:        info.Enabled,
		LastScanAt:     info.LastScanAt,
		LastError:      info.LastError,
		DocumentCount:  info.Stats.Documents,
		ChunkCount:     info.Stats.Chunks,
		PendingCount:   info.Stats.Pending,
		Directories:    dirs,
		EmbeddingModel: info.EmbedModel,
		IndexedModels:  models,
	}
}

func documentView(doc Document, withContent bool) DocumentView {
	view := DocumentView{
		ID:          doc.ID,
		SourceID:    doc.SourceID,
		Origin:      doc.Origin,
		RelPath:     doc.RelPath,
		Title:       doc.Title,
		DocType:     doc.DocType,
		ByteSize:    doc.ByteSize,
		Status:      doc.Status,
		ErrorDetail: doc.ErrorDetail,
		Generation:  doc.Generation,
		Mtime:       doc.Mtime,
		UpdatedAt:   doc.UpdatedAt,
	}
	if withContent && doc.Origin == OriginInline {
		content := doc.Content
		view.Content = &content
	}
	return view
}

func skipViews(skips []ScanSkip) []ScanSkipView {
	out := make([]ScanSkipView, 0, len(skips))
	for _, skip := range skips {
		out = append(out, ScanSkipView(skip))
	}
	return out
}

func staticScanError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalidSourceRoot):
		return "Invalid knowledge source directory"
	case errors.Is(err, ErrPrivateBaseNotSetup):
		return "Private knowledge directory is not configured"
	default:
		return "Source scan failed"
	}
}
