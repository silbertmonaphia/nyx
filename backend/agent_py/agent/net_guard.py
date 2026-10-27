"""SSRF guard for the agent's outbound dials.

`validate_target` backs the fetch/search web tools (openspec/changes/
agent-web-tools); `ensure_public_address` backs the BYOK chat-LLM dial,
where the endpoint may use any port and must stay proxy-friendly.

Mirrors the Go backend's two-layer pattern (`checkHostNotPrivate` in
`internal/platform/config` + the per-`DialContext` re-check in the vLLM
client): validate the URL shape, resolve the host, check EVERY returned
address against the deny-list, then hand the caller a `Target` whose
`pinned_ip` is the validated address to dial. The fetch layer builds its
requests from `Target.request_url()` (pinned IP in the URL) with the
original hostname carried in the `Host` header and the `sni_hostname`
request extension — so a name that re-resolves to a denied address between
validation and connection (DNS rebinding) cannot be reached: the dial
target is the validated address, and TLS still verifies the certificate
against the original hostname.

Two failure classes, deliberately distinct so `web_tools` can map them onto
the executor's retry taxonomy:

  - `WebUrlError`      — policy/syntax rejection; the model can correct the
                         URL → arg-class (model re-prompt).
  - `WebResolveError`  — DNS resolution failed; transport-class → transient
                         retry (spec: "DNS failure" is in the transient list).
"""

from __future__ import annotations

import asyncio
import ipaddress
import socket
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from urllib.parse import urlsplit

ALLOWED_SCHEMES = frozenset({"http", "https"})
ALLOWED_PORTS = frozenset({80, 443})
DEFAULT_PORTS = {"http": 80, "https": 443}

# Async `(host, port) -> [ip, ...]`. Tests inject a double; production uses
# the event loop's getaddrinfo (non-blocking, threads under the hood).
Resolver = Callable[[str, int], Awaitable[list[str]]]


class WebUrlError(Exception):
    """Policy/syntax rejection — correctable (arg-class) in the executor."""


class WebResolveError(Exception):
    """DNS resolution failure — transient (retryable) in the executor."""


def is_denied_address(ip: ipaddress.IPv4Address | ipaddress.IPv6Address) -> bool:
    """True for addresses that must never be dialed by `web_fetch`.

    Covers loopback, RFC1918 private, link-local (incl. the cloud metadata
    address 169.254.169.254), multicast, unspecified, and reserved classes
    for IPv4 and IPv6 — including IPv4-mapped IPv6 forms and IPv6
    unique-local (fc00::/7, inside `is_private`).
    """
    if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped is not None:
        ip = ip.ipv4_mapped
    return bool(
        ip.is_loopback
        or ip.is_private
        or ip.is_link_local
        or ip.is_multicast
        or ip.is_unspecified
        or ip.is_reserved
    )


async def _default_resolver(host: str, port: int) -> list[str]:
    loop = asyncio.get_running_loop()
    infos = await loop.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    return sorted({str(info[4][0]) for info in infos})


@dataclass(frozen=True)
class Target:
    """A validated fetch destination: original URL identity + pinned dial address."""

    url: str
    scheme: str
    host: str  # original hostname — Host header + TLS SNI
    port: int
    pinned_ip: str  # validated address the connection must dial
    path_query: str

    @property
    def is_https(self) -> bool:
        return self.scheme == "https"

    @property
    def request_url(self) -> str:
        """URL whose *host is the pinned IP* — the dial target cannot be
        re-resolved by the transport layer."""
        literal = f"[{self.pinned_ip}]" if ":" in self.pinned_ip else self.pinned_ip
        return f"{self.scheme}://{literal}:{self.port}{self.path_query}"

    @property
    def host_header(self) -> str:
        """`Host:` header value — the original hostname (with port when
        non-default), as if the request had gone to the hostname itself."""
        default = DEFAULT_PORTS[self.scheme]
        return self.host if self.port == default else f"{self.host}:{self.port}"

    def request_extensions(self) -> dict:
        """httpx/httpcore request extensions: keep TLS SNI (and therefore
        certificate verification) on the original hostname even though the
        URL host is the pinned IP."""
        if self.is_https:
            return {"sni_hostname": self.host}
        return {}


def _pick_pin(addresses: list[ipaddress.IPv4Address | ipaddress.IPv6Address]) -> str:
    """Prefer an IPv4 address when both families are available (better
    connectivity in the typical dev/deploy environment); else the first
    validated address in resolver order."""
    for ip in addresses:
        if ip.version == 4:
            return str(ip)
    return str(addresses[0])


async def validate_target(
    url: str,
    *,
    allow_private: bool = False,
    resolver: Resolver | None = None,
) -> Target:
    """Validate `url` and return the pinned `Target` to dial.

    Raises `WebUrlError` for scheme/port/userinfo/host violations and for
    any resolved address in a denied class (unless `allow_private`), and
    `WebResolveError` when the host does not resolve.
    """
    try:
        parts = urlsplit(url)
    except ValueError as exc:
        # urlsplit raises for malformed bracket literals (e.g.
        # `http://[::1/x` → "Invalid IPv6 URL"). Untyped here would escape
        # as non_retryable; the spec names malformed URLs correctable.
        raise WebUrlError(f"malformed URL: {exc}") from exc
    if parts.scheme not in ALLOWED_SCHEMES:
        raise WebUrlError(
            f"only http/https URLs are allowed; got scheme {parts.scheme or '(none)'!r}"
        )
    if parts.username is not None or parts.password is not None:
        raise WebUrlError("URLs carrying credentials (user:pass@host) are not allowed")
    host = parts.hostname
    if not host:
        raise WebUrlError("URL has no host")
    try:
        port = parts.port
    except ValueError:
        raise WebUrlError("URL has an invalid port") from None
    if port is None:
        port = DEFAULT_PORTS[parts.scheme]
    if not allow_private and port not in ALLOWED_PORTS:
        raise WebUrlError(f"only ports 80/443 are allowed; got {port}")

    resolve = resolver or _default_resolver
    try:
        raw_addresses = await resolve(host, port)
    except (socket.gaierror, OSError, UnicodeError) as exc:
        # UnicodeError (a ValueError, not OSError) is what getaddrinfo
        # raises for an over-long/non-encodable label — a malformed-host
        # shape, same transient-class treatment as other resolution
        # failures so it never escapes as an untyped error.
        raise WebResolveError(f"could not resolve host {host!r}") from exc

    parsed: list[ipaddress.IPv4Address | ipaddress.IPv6Address] = []
    for raw in raw_addresses:
        try:
            parsed.append(ipaddress.ip_address(raw))
        except ValueError:
            continue
    if not parsed:
        raise WebResolveError(f"could not resolve host {host!r}")

    if not allow_private:
        for ip in parsed:
            if is_denied_address(ip):
                # Name the CLASS, not the address (mirrors the Go guard's
                # config.go refusal text): the message reaches the model
                # and the browser UI, and echoing the resolved IP would
                # let a tenant enumerate internal hostnames / infer the
                # cluster service CIDR.
                raise WebUrlError(
                    f"target {host!r} resolves to a non-public address; refusing"
                )

    path_query = parts.path or "/"
    if parts.query:
        path_query = f"{path_query}?{parts.query}"
    return Target(
        url=url,
        scheme=parts.scheme,
        host=host,
        port=port,
        pinned_ip=_pick_pin(parsed),
        path_query=path_query,
    )


async def ensure_public_address(
    url: str,
    *,
    allow_private: bool = False,
    resolver: Resolver | None = None,
) -> None:
    """Refuse a URL whose host resolves to a denied address class.

    Unlike `validate_target` this is the LLM-endpoint policy: no 80/443
    port rule (compatible endpoints routinely listen on other ports) and
    no address pinning (the caller dials the hostname so proxy egress
    keeps working).
    """
    try:
        parts = urlsplit(url)
    except ValueError as exc:
        raise WebUrlError(f"malformed URL: {exc}") from exc
    if parts.scheme not in ALLOWED_SCHEMES:
        raise WebUrlError(
            f"only http/https URLs are allowed; got scheme {parts.scheme or '(none)'!r}"
        )
    if parts.username is not None or parts.password is not None:
        raise WebUrlError("URLs carrying credentials (user:pass@host) are not allowed")
    host = parts.hostname
    if not host:
        raise WebUrlError("URL has no host")
    try:
        port = parts.port
    except ValueError:
        raise WebUrlError("URL has an invalid port") from None
    if port is None:
        port = DEFAULT_PORTS[parts.scheme]

    resolve = resolver or _default_resolver
    try:
        raw_addresses = await resolve(host, port)
    except (socket.gaierror, OSError, UnicodeError) as exc:
        raise WebResolveError(f"could not resolve host {host!r}") from exc

    parsed: list[ipaddress.IPv4Address | ipaddress.IPv6Address] = []
    for raw in raw_addresses:
        try:
            parsed.append(ipaddress.ip_address(raw))
        except ValueError:
            continue
    if not parsed:
        raise WebResolveError(f"could not resolve host {host!r}")
    if allow_private:
        return
    for ip in parsed:
        if is_denied_address(ip):
            raise WebUrlError(
                f"LLM endpoint {host!r} resolves to a non-public address; refusing"
            )
