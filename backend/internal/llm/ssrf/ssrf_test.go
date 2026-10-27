package ssrf

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIPClassRefused_Classes(t *testing.T) {
	cases := []struct {
		ip      string
		refused bool
		reason  string
	}{
		{"127.0.0.1", true, "loopback"},
		{"::1", true, "loopback"},
		{"10.1.2.3", true, "private"},
		{"172.16.0.1", true, "private"},
		{"192.168.1.1", true, "private"},
		{"fd00::1", true, "private"},
		{"169.254.169.254", true, "link-local"},
		{"fe80::1", true, "link-local"},
		{"0.0.0.0", true, "unspecified"},
		{"224.0.0.1", true, "multicast"},
		{"ff01::1", true, "multicast"},
		{"::ffff:127.0.0.1", true, "loopback"},
		{"8.8.8.8", false, ""},
		{"2001:4860:4860::8888", false, ""},
	}
	for _, tc := range cases {
		refused, reason := IPClassRefused(net.ParseIP(tc.ip))
		if refused != tc.refused {
			t.Fatalf("%s: refused = %v, want %v", tc.ip, refused, tc.refused)
		}
		if refused && !strings.Contains(reason, tc.reason) {
			t.Fatalf("%s: reason = %q, want prefix %q", tc.ip, reason, tc.reason)
		}
	}
}

func TestCheckHost_LiteralLoopbackRefused(t *testing.T) {
	err := CheckHost("127.0.0.1", "TEST_URL")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("want loopback refusal, got %v", err)
	}
}

func TestCheckHost_LiteralRFC1918Refused(t *testing.T) {
	err := CheckHost("10.0.0.1", "TEST_URL")
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("want private refusal, got %v", err)
	}
}

func TestCheckHost_MetadataIPRefused(t *testing.T) {
	err := CheckHost("169.254.169.254", "TEST_URL")
	if err == nil || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("want link-local refusal, got %v", err)
	}
}

func TestCheckHost_DNSRefusedClasses(t *testing.T) {
	orig := ResolveHost
	ResolveHost = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.168.0.9")}, nil
	}
	t.Cleanup(func() { ResolveHost = orig })

	err := CheckHost("rebind.example", "TEST_URL")
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("want private refusal via DNS, got %v", err)
	}
}

func TestCheckHost_DNSFailureIsError(t *testing.T) {
	orig := ResolveHost
	ResolveHost = func(host string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { ResolveHost = orig })

	err := CheckHost("missing.example", "TEST_URL")
	if err == nil || !strings.Contains(err.Error(), "DNS lookup failed") {
		t.Fatalf("want DNS failure error, got %v", err)
	}
}

func TestCheckHost_LabelAppearsInMessages(t *testing.T) {
	err := CheckHost("127.0.0.1", "llm_base_url")
	if err == nil || !strings.Contains(err.Error(), "llm_base_url") {
		t.Fatalf("want label in message, got %v", err)
	}
}

func TestCheckHost_AllowsPublicLiteral(t *testing.T) {
	if err := CheckHost("8.8.8.8", "TEST_URL"); err != nil {
		t.Fatalf("want public literal accepted, got %v", err)
	}
}

func TestCheckHost_EmptyHostRefused(t *testing.T) {
	err := CheckHost("", "TEST_URL")
	if err == nil || !strings.Contains(err.Error(), "host is empty") {
		t.Fatalf("want empty-host error, got %v", err)
	}
}

func TestNewTransport_RefusesPrivateTarget(t *testing.T) {
	transport := NewTransport(false, "TEST_URL")
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("want private refusal, got %v", err)
	}
}

func TestNewTransport_LabelAppearsInMessage(t *testing.T) {
	transport := NewTransport(false, "fallback_base_url")
	req, err := http.NewRequest(http.MethodGet, "http://10.0.0.1/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "fallback_base_url") {
		t.Fatalf("want label in refusal, got %v", err)
	}
}

func TestNewTransport_UnresolvableTargetPasses(t *testing.T) {
	orig := ResolveHost
	ResolveHost = func(host string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { ResolveHost = orig })

	transport := NewTransport(false, "TEST_URL")
	req, err := http.NewRequest(http.MethodGet, "http://target.example/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("an unresolvable target must not be classified as private, got %v", err)
	}
}

func TestNewTransport_ProxyOnlyEgressStillChecksTarget(t *testing.T) {
	proxyURL := startProxy(t)
	t.Setenv("HTTP_PROXY", proxyURL)
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("NO_PROXY", "")

	transport := NewTransport(false, "TEST_URL")
	req, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("want private refusal through a proxy, got %v", err)
	}
}

func TestNewTransport_ProxyHostIsNotTreatedAsTheTarget(t *testing.T) {
	proxyURL := startProxy(t)
	t.Setenv("HTTP_PROXY", proxyURL)
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("NO_PROXY", "")

	orig := ResolveHost
	ResolveHost = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { ResolveHost = orig })

	transport := NewTransport(false, "TEST_URL")
	req, err := http.NewRequest(http.MethodGet, "http://public.example/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		if errors.Is(err, ErrPrivateAddress) || strings.Contains(err.Error(), "TEST_URL") {
			t.Fatalf("the loopback proxy must not be judged as the target, got %v", err)
		}
		return
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
}

func startProxy(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return "http://" + listener.Addr().String()
}

func TestNewHTTPClient_ConstructionCheckRefusesPrivate(t *testing.T) {
	parsed, err := url.Parse("http://127.0.0.1:8000/v1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewHTTPClient(parsed, 0, false, "TEST_URL")
	if err == nil || !strings.Contains(err.Error(), "TEST_URL") {
		t.Fatalf("want construction-time refusal, got %v", err)
	}
}

func TestNewHTTPClient_ZeroTimeoutSetsNone(t *testing.T) {
	parsed, err := url.Parse("http://8.8.8.8/v1")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewHTTPClient(parsed, 0, false, "TEST_URL")
	if err != nil {
		t.Fatalf("want public URL accepted, got %v", err)
	}
	if client.Timeout != 0 {
		t.Fatalf("want zero client timeout for streaming, got %v", client.Timeout)
	}
}

func TestNewHTTPClient_TimeoutPreserved(t *testing.T) {
	parsed, err := url.Parse("http://8.8.8.8/v1")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewHTTPClient(parsed, 2*time.Second, false, "TEST_URL")
	if err != nil {
		t.Fatalf("want public URL accepted, got %v", err)
	}
	if client.Timeout != 2*time.Second {
		t.Fatalf("want timeout preserved, got %v", client.Timeout)
	}
}

func TestCheckHostBestEffort_LiteralPrivateRefused(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1"} {
		err := CheckHostBestEffort(host, "TEST_URL")
		if err == nil {
			t.Fatalf("%s: want private refusal, got nil", host)
		}
	}
}

func TestCheckHostBestEffort_PublicLiteralAllowed(t *testing.T) {
	if err := CheckHostBestEffort("8.8.8.8", "TEST_URL"); err != nil {
		t.Fatalf("want public literal accepted, got %v", err)
	}
}

func TestCheckHostBestEffort_DNSFailureDoesNotBlock(t *testing.T) {
	orig := ResolveHost
	ResolveHost = func(host string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { ResolveHost = orig })

	if err := CheckHostBestEffort("unresolved.example", "TEST_URL"); err != nil {
		t.Fatalf("an unresolvable host must not block the caller, got %v", err)
	}
}

func TestCheckHostBestEffort_DNSPrivateRefused(t *testing.T) {
	orig := ResolveHost
	ResolveHost = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.168.0.9")}, nil
	}
	t.Cleanup(func() { ResolveHost = orig })

	err := CheckHostBestEffort("rebind.example", "TEST_URL")
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("want private refusal via DNS, got %v", err)
	}
}
