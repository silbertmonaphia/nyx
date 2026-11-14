package kb

import (
	"errors"
	"net/http"

	"nyx/internal/platform/api"
)

var (
	ErrSourceNotFound      = errors.New("kb: source not found")
	ErrDocumentNotFound    = errors.New("kb: document not found")
	ErrInvalidSourceRoot   = errors.New("kb: invalid source root")
	ErrSharedSourceLocked  = errors.New("kb: shared source is operator-managed")
	ErrFileBackedDocument  = errors.New("kb: document is file-backed")
	ErrNotText             = errors.New("kb: upload is not text")
	ErrDisabled            = errors.New("kb: knowledge base is disabled")
	ErrEmptyBody           = errors.New("kb: document body is required")
	ErrInvalidSourceName   = errors.New("kb: invalid source name")
	ErrInvalidSourceUpdate = errors.New("kb: invalid source update")
	ErrInvalidDocName      = errors.New("kb: invalid document name")
	ErrEmbedderUnavailable = errors.New("kb: embedder is not configured")
)

func init() {
	api.RegisterSentinel(ErrSourceNotFound, http.StatusNotFound, "Knowledge source not found")
	api.RegisterSentinel(ErrDocumentNotFound, http.StatusNotFound, "Knowledge document not found")
	api.RegisterSentinel(ErrInvalidSourceRoot, http.StatusBadRequest, "Invalid knowledge source directory")
	api.RegisterSentinel(ErrSharedSourceLocked, http.StatusForbidden, "The shared knowledge source is operator-managed")
	api.RegisterSentinel(ErrFileBackedDocument, http.StatusConflict, "This document is managed by its source file")
	api.RegisterSentinel(ErrNotText, http.StatusBadRequest, "Only text or Markdown documents can be uploaded")
	api.RegisterSentinel(ErrDisabled, http.StatusNotFound, "Knowledge base is not enabled")
	api.RegisterSentinel(ErrEmptyBody, http.StatusBadRequest, "Document body is required")
	api.RegisterSentinel(ErrInvalidSourceName, http.StatusBadRequest, "Invalid knowledge source name")
	api.RegisterSentinel(ErrInvalidSourceUpdate, http.StatusBadRequest, "Invalid knowledge source update")
	api.RegisterSentinel(ErrInvalidDocName, http.StatusBadRequest, "Invalid document name")
}
