package chat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nyx/internal/platform/api"
	"nyx/internal/reqctx"
)

const runsBasePath = "/agent/runs"

const (
	runStreamDisabledMessage    = "Run streams are unavailable"
	runStreamDisabledDetail     = "The agent service is disabled"
	runStreamUnreachableMessage = "Agent run stream unavailable"
	runStreamUnreachableDetail  = "The agent service could not be reached"
	runStreamCopyFailedDetail   = "Agent run stream failed"

	runCancelDisabledMessage    = "Run cancellation is unavailable"
	runCancelDisabledDetail     = "The agent service is disabled"
	runCancelUnreachableMessage = "Agent run cancellation unavailable"
	runCancelUnreachableDetail  = "The agent service could not be reached"
)

func runStreamPath(runID string) string {
	return runsBasePath + "/" + url.PathEscape(runID) + "/stream"
}

func runCancelPath(runID string) string {
	return runsBasePath + "/" + url.PathEscape(runID) + "/cancel"
}

func (p *AgentProxy) RunStream(ctx context.Context, runID string, userID int, bearerToken string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+runStreamPath(runID), nil)
	if err != nil {
		return nil, ErrAgentUnavailable
	}
	httpReq.Header.Set("X-Nyx-User-Id", strconv.Itoa(userID))
	httpReq.Header.Set(ServiceTokenHeader, p.serviceToken)
	if bearerToken != "" {
		httpReq.Header.Set("Authorization", bearerToken)
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, ErrAgentUnavailable
	}
	if resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented {
		_ = resp.Body.Close()
		return nil, ErrAgentUnavailable
	}
	return resp, nil
}

func (h *Handler) runStream(w http.ResponseWriter, r *http.Request) {
	userID := reqctx.UserIDFromContext(r.Context())
	if userID == 0 {
		api.WriteError(w, r, http.StatusUnauthorized, "Authentication required", nil)
		return
	}
	if h.service == nil || !h.service.IsAgentEnabled() {
		h.writeRunGate503(w, r, runStreamDisabledMessage, runStreamDisabledDetail)
		return
	}

	bearer := r.Header.Get("Authorization")
	ctx, cancel := context.WithTimeout(r.Context(), h.service.MaxStreamDuration())
	defer cancel()

	resp, err := h.service.Proxy().RunStream(ctx, chi.URLParam(r, "run_id"), userID, bearer)
	if err != nil {
		h.writeRunUnavailable502(w, r, err, runStreamUnreachableMessage, runStreamUnreachableDetail)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeSessionResponse(w, r, resp)
		return
	}

	writeSSEHeaders(w)
	forwardRunStream(w, r, resp.Body)
}

func (h *Handler) runCancel(w http.ResponseWriter, r *http.Request) {
	userID := reqctx.UserIDFromContext(r.Context())
	if userID == 0 {
		api.WriteError(w, r, http.StatusUnauthorized, "Authentication required", nil)
		return
	}
	if h.sessions == nil || h.service == nil || !h.service.IsAgentEnabled() {
		h.writeRunGate503(w, r, runCancelDisabledMessage, runCancelDisabledDetail)
		return
	}

	resp, err := h.sessions.Forward(r.Context(), sessionProxyRequest{
		method: http.MethodPost,
		path:   runCancelPath(chi.URLParam(r, "run_id")),
		userID: userID,
		bearer: r.Header.Get("Authorization"),
	})
	if err != nil {
		h.writeRunUnavailable502(w, r, err, runCancelUnreachableMessage, runCancelUnreachableDetail)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		api.WriteError(w, r, http.StatusBadGateway, runCancelUnreachableMessage, runCancelUnreachableDetail)
		return
	}
	writeSessionResponse(w, r, resp)
}

func (h *Handler) writeRunGate503(w http.ResponseWriter, r *http.Request, disabledMessage, disabledDetail string) {
	detail := disabledDetail
	if h.service != nil && h.service.AgentMisconfigured() {
		detail = agentMisconfiguredDetail
	}
	api.WriteError(w, r, http.StatusServiceUnavailable, disabledMessage, detail)
}

func (h *Handler) writeRunUnavailable502(w http.ResponseWriter, r *http.Request, err error, message, detail string) {
	if r.Context().Err() != nil {
		return
	}
	api.WriteError(w, r, http.StatusBadGateway, message,
		api.ClassifyAndLog(r.Context(), err, detail))
}

func writeSSEHeaders(w http.ResponseWriter) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func forwardRunStream(w http.ResponseWriter, r *http.Request, body io.Reader) {
	fw := &flushingWriter{w: w}
	if sawTerminal, err := copyAndFlush(fw, body); err != nil {
		if sawTerminal {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
			api.ClassifyAndLog(r.Context(), err, runStreamCopyFailedDetail)
			emitStreamTimeout(fw, r.Context())
			return
		}
		if !errors.Is(err, context.Canceled) && r.Context().Err() == nil {
			api.ClassifyAndLog(r.Context(), err, runStreamCopyFailedDetail)
		}
	}
}
