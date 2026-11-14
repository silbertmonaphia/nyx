package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

type CapturedRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type RequestCapture struct {
	mu     sync.Mutex
	inner  http.RoundTripper
	doer   Doer
	got    *CapturedRequest
	status int
}

type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

func (c *RequestCapture) Wrap(inner http.RoundTripper) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	c.inner = inner
	return c
}

func (c *RequestCapture) WrapDoer(inner Doer) Doer {
	c.doer = inner
	return c
}

func (c *RequestCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	c.record(req)
	resp, err := c.inner.RoundTrip(req)
	c.recordStatus(resp)
	return resp, err
}

func (c *RequestCapture) Do(req *http.Request) (*http.Response, error) {
	c.record(req)
	var resp *http.Response
	var err error
	if c.doer != nil {
		resp, err = c.doer.Do(req)
	} else {
		resp, err = http.DefaultClient.Do(req)
	}
	c.recordStatus(resp)
	return resp, err
}

func (c *RequestCapture) recordStatus(resp *http.Response) {
	if resp == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = resp.StatusCode
}

func (c *RequestCapture) LastStatus() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *RequestCapture) record(req *http.Request) {
	headers := map[string]string{}
	for k, vals := range req.Header {
		headers[k] = MaskCredential(k, vals)
	}
	var body json.RawMessage
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err == nil {
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(raw))
			if json.Valid(raw) {
				body = json.RawMessage(raw)
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = &CapturedRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: headers,
		Body:    body,
	}
}

func (c *RequestCapture) Captured() *CapturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got
}

func MaskCredential(key string, vals []string) string {
	v := ""
	if len(vals) > 0 {
		v = vals[0]
	}
	if key != "Authorization" {
		return v
	}
	for _, scheme := range []string{"Bearer ", "bearer ", "Basic ", "basic "} {
		if len(v) > len(scheme) && v[:len(scheme)] == scheme {
			rest := v[len(scheme):]
			if len(rest) <= 4 {
				return scheme[:len(scheme)-1] + " ••••"
			}
			return scheme[:len(scheme)-1] + " ••••" + rest[len(rest)-4:]
		}
	}
	return "••••"
}
