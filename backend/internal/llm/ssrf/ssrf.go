package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

var ErrPrivateAddress = errors.New("private/restricted address")

var ResolveHost = func(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return net.LookupIP(host)
}

func IPClassRefused(ip net.IP) (bool, string) {
	switch {
	case ip.IsUnspecified():
		return true, "unspecified (0.0.0.0 / ::)"
	case ip.IsLoopback():
		return true, "loopback (127.0.0.0/8 / ::1)"
	case ip.IsPrivate():
		return true, "private (RFC1918 / ULA)"
	case ip.IsLinkLocalUnicast():
		return true, "link-local unicast (169.254/16 / fe80::/10)"
	case ip.IsLinkLocalMulticast():
		return true, "link-local multicast"
	case ip.IsInterfaceLocalMulticast():
		return true, "interface-local multicast (ff01::/16)"
	case ip.IsMulticast():
		return true, "multicast"
	}
	return false, ""
}

func CheckHost(host, label string) error {
	if host == "" {
		return fmt.Errorf("%s host is empty", label)
	}
	addrs, err := ResolveHost(host)
	if err != nil {
		return fmt.Errorf("%s DNS lookup failed for %q: %w", label, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%s DNS lookup returned no addresses for %q", label, host)
	}
	for _, ip := range addrs {
		if refused, reason := IPClassRefused(ip); refused {
			return fmt.Errorf("%s points at a %w (%s)", label, ErrPrivateAddress, reason)
		}
	}
	return nil
}

func CheckHostBestEffort(host, label string) error {
	if host == "" {
		return fmt.Errorf("%s host is empty", label)
	}
	addrs, err := ResolveHost(host)
	if err != nil || len(addrs) == 0 {
		return nil
	}
	for _, ip := range addrs {
		if refused, reason := IPClassRefused(ip); refused {
			return fmt.Errorf("%s points at a %w (%s)", label, ErrPrivateAddress, reason)
		}
	}
	return nil
}

type Transport struct {
	allowPrivate bool
	label        string
	direct       *http.Transport
	proxied      *http.Transport
}

func NewTransport(allowPrivate bool, label string) *Transport {
	return &Transport{
		allowPrivate: allowPrivate,
		label:        label,
		direct:       baseTransport(allowPrivate, label, nil),
		proxied:      baseTransport(true, label, http.ProxyFromEnvironment),
	}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.allowPrivate {
		if err := CheckHostBestEffort(req.URL.Hostname(), t.label); err != nil {
			return nil, err
		}
	}
	if proxyURL, _ := http.ProxyFromEnvironment(req); proxyURL != nil {
		return t.proxied.RoundTrip(req)
	}
	return t.direct.RoundTrip(req)
}

func (t *Transport) CloseIdleConnections() {
	t.direct.CloseIdleConnections()
	t.proxied.CloseIdleConnections()
}

func baseTransport(allowPrivate bool, label string, proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	return &http.Transport{
		Proxy: proxy,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, &net.OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: err}
			}
			if !allowPrivate {
				if err := CheckHost(host, label); err != nil {
					return nil, &net.OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: err}
				}
			}
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func NewHTTPClient(parsed *url.URL, timeout time.Duration, allowPrivate bool, label string) (*http.Client, error) {
	if !allowPrivate {
		if err := CheckHost(parsed.Hostname(), label); err != nil {
			return nil, err
		}
	}
	return &http.Client{
		Transport: NewTransport(allowPrivate, label),
		Timeout:   timeout,
	}, nil
}
