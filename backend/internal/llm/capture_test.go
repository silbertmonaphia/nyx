package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaskCredential(t *testing.T) {
	cases := []struct {
		name string
		key  string
		vals []string
		want string
	}{
		{"bearer long", "Authorization", []string{"Bearer sk-abcdef-12345678"}, "Bearer ••••5678"},
		{"bearer short is fully hidden", "Authorization", []string{"Bearer abc"}, "Bearer ••••"},
		{"bearer exactly four", "Authorization", []string{"Bearer abcd"}, "Bearer ••••"},
		{"basic long", "Authorization", []string{"Basic dXNlcjpwYXNz"}, "Basic ••••YXNz"},
		{"lowercase bearer", "Authorization", []string{"bearer sk-abcdef-12345678"}, "bearer ••••5678"},
		{"unknown scheme fully hidden", "Authorization", []string{"Token abcdefgh"}, "••••"},
		{"empty authorization", "Authorization", nil, "••••"},
		{"non-auth header passes through", "Content-Type", []string{"application/json"}, "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskCredential(tc.key, tc.vals); got != tc.want {
				t.Fatalf("MaskCredential(%q, %v) = %q, want %q", tc.key, tc.vals, got, tc.want)
			}
		})
	}
}

func TestRequestCaptureRecordsRequestAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	t.Cleanup(srv.Close)

	capture := &RequestCapture{}
	client := &http.Client{Transport: capture.Wrap(http.DefaultTransport)}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/embeddings", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-planted-secret-9876")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got := capture.Captured()
	if got == nil {
		t.Fatal("capture recorded nothing")
	}
	if got.Method != http.MethodPost {
		t.Fatalf("method = %q", got.Method)
	}
	if got.URL != srv.URL+"/v1/embeddings" {
		t.Fatalf("url = %q", got.URL)
	}
	if got.Headers["Authorization"] != "Bearer ••••9876" {
		t.Fatalf("authorization not masked: %q", got.Headers["Authorization"])
	}
	if got.Headers["Content-Type"] != "application/json" {
		t.Fatalf("content-type = %q", got.Headers["Content-Type"])
	}
	if string(got.Body) != `{"model":"m"}` {
		t.Fatalf("body = %s", got.Body)
	}
	if status := capture.LastStatus(); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestRequestCaptureDoerPathRecordsStatus(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Tavily-Access-Mode")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	capture := &RequestCapture{}
	doer := capture.WrapDoer(srv.Client())
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/search", strings.NewReader(`{"query":"x","max_results":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tavily-Access-Mode", "keyless")
	resp, err := doer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if gotAuth != "keyless" {
		t.Fatalf("upstream saw %q, the probe must have run for the assertion below to mean anything", gotAuth)
	}
	if status := capture.LastStatus(); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if capture.Captured().Headers["X-Tavily-Access-Mode"] != "keyless" {
		t.Fatalf("keyless header missing from disclosure")
	}
}
