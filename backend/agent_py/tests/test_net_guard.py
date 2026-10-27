"""SSRF-guard tests — openspec/changes/agent-web-tools tasks 1.1–1.3.

Every address-class case uses an injected resolver double; no live DNS.
The rebind case (1.3) proves the dial target comes from the validated
address: `Target.request_url` carries the pinned IP while `host_header`
and the `sni_hostname` extension carry the original hostname, so no
second resolution ever feeds the connection.
"""

from __future__ import annotations

import ipaddress
import socket
from dataclasses import FrozenInstanceError

import pytest

from agent.net_guard import (
    Target,
    WebResolveError,
    WebUrlError,
    ensure_public_address,
    is_denied_address,
    validate_target,
)


def resolver_for(*addresses: str):
    async def _resolve(host: str, port: int) -> list[str]:
        return list(addresses)

    return _resolve


# --- 1.1 URL shape --------------------------------------------------------


@pytest.mark.parametrize("url", ["file:///etc/passwd", "ftp://example.com/x", "gopher://x"])
async def test_non_http_scheme_refused(url):
    with pytest.raises(WebUrlError, match="http/https"):
        await validate_target(url, resolver=resolver_for("93.184.216.34"))


async def test_userinfo_refused():
    with pytest.raises(WebUrlError, match="credentials"):
        await validate_target(
            "http://user:pass@example.com/", resolver=resolver_for("93.184.216.34")
        )


async def test_missing_host_refused():
    with pytest.raises(WebUrlError, match="no host"):
        await validate_target("http:///path", resolver=resolver_for("93.184.216.34"))


async def test_invalid_port_refused():
    with pytest.raises(WebUrlError, match="invalid port"):
        await validate_target("http://example.com:99999/", resolver=resolver_for("93.184.216.34"))


async def test_nonstandard_port_refused():
    with pytest.raises(WebUrlError, match="80/443"):
        await validate_target("http://example.com:8080/", resolver=resolver_for("93.184.216.34"))


async def test_nonstandard_port_allowed_with_allow_private():
    """Spec: allow-private lifts the port rule too (fixture servers don't
    run on 80/443)."""
    target = await validate_target(
        "http://127.0.0.1:8080/fixture",
        allow_private=True,
        resolver=resolver_for("127.0.0.1"),
    )
    assert target.port == 8080
    assert target.pinned_ip == "127.0.0.1"


async def test_default_ports_applied():
    t_http = await validate_target("http://example.com/", resolver=resolver_for("93.184.216.34"))
    t_https = await validate_target("https://example.com/", resolver=resolver_for("93.184.216.34"))
    assert t_http.port == 80
    assert t_https.port == 443


async def test_resolution_failure_is_transient_class():
    async def _boom(host: str, port: int) -> list[str]:
        raise socket.gaierror(-2, "Name or service not known")

    with pytest.raises(WebResolveError):
        await validate_target("http://no-such-host.invalid/", resolver=_boom)


# --- 1.2 address deny-list ------------------------------------------------


@pytest.mark.parametrize(
    "address",
    [
        "127.0.0.1",  # loopback
        "10.1.2.3",  # RFC1918
        "172.16.5.5",  # RFC1918
        "192.168.1.1",  # RFC1918
        "169.254.169.254",  # link-local / cloud metadata
        "224.0.0.1",  # multicast
        "0.0.0.0",  # unspecified
        "::1",  # IPv6 loopback
        "fe80::1",  # IPv6 link-local
        "fc00::1",  # IPv6 unique-local
        "::ffff:10.0.0.1",  # IPv4-mapped RFC1918
        "::ffff:127.0.0.1",  # IPv4-mapped loopback
    ],
)
async def test_denied_address_classes_refused(address):
    with pytest.raises(WebUrlError, match="non-public"):
        await validate_target("http://evil.test/", resolver=resolver_for(address))


async def test_mixed_public_and_private_refused():
    """One private address in the answer set taints the whole target —
    rotation/rebinding via multi-A records cannot smuggle a denied hop."""
    with pytest.raises(WebUrlError, match="non-public"):
        await validate_target(
            "http://mixed.test/", resolver=resolver_for("93.184.216.34", "10.0.0.9")
        )


async def test_public_address_accepted_and_pinned():
    target = await validate_target(
        "https://example.com/page?q=1", resolver=resolver_for("93.184.216.34")
    )
    assert target.pinned_ip == "93.184.216.34"
    assert target.host == "example.com"


async def test_ipv4_preferred_when_both_families_present():
    target = await validate_target(
        "http://dual.test/", resolver=resolver_for("2606:2800:220:1:248:1893:25c8:1946", "93.184.216.34")
    )
    assert target.pinned_ip == "93.184.216.34"


async def test_allow_private_permits_denied_classes():
    target = await validate_target(
        "http://127.0.0.1:9000/", allow_private=True, resolver=resolver_for("127.0.0.1")
    )
    assert target.pinned_ip == "127.0.0.1"


def test_is_denied_address_direct():
    assert is_denied_address(ipaddress.ip_address("169.254.169.254"))
    assert is_denied_address(ipaddress.ip_address("::ffff:192.168.0.1"))
    assert not is_denied_address(ipaddress.ip_address("93.184.216.34"))


# --- 1.3 dial pinning (rebind defense) ------------------------------------


async def test_dial_target_is_the_validated_address_not_the_hostname():
    """The request URL must carry the pinned IP; the original hostname
    survives only in the Host header + SNI extension. A transport doing a
    fresh resolve of the URL host would dial the pinned IP anyway — there
    is no hostname left to re-resolve."""
    target = await validate_target(
        "https://example.com/a/b?c=d", resolver=resolver_for("93.184.216.34")
    )
    assert target.request_url == "https://93.184.216.34:443/a/b?c=d"
    assert target.host_header == "example.com"
    assert target.request_extensions() == {"sni_hostname": "example.com"}


async def test_rebind_double_resolve_cannot_swap_the_pin():
    """Resolver returns a public address at validation; a hypothetical
    second resolution would return a private one. The returned Target
    keeps the validated public pin — the fetch layer never calls the
    resolver again for the same hop."""
    calls = []

    async def _flaky(host: str, port: int) -> list[str]:
        calls.append(host)
        if len(calls) == 1:
            return ["93.184.216.34"]
        return ["127.0.0.1"]

    target = await validate_target("http://rebind.test/", resolver=_flaky)
    assert target.pinned_ip == "93.184.216.34"
    assert calls == ["rebind.test"]  # resolved exactly once


async def test_ipv6_pin_bracketed_and_nondefault_port_in_host_header():
    target = await validate_target(
        "http://[2606:2800:220:1:248:1893:25c8:1946]:8080/x",
        allow_private=True,
        resolver=resolver_for("2606:2800:220:1:248:1893:25c8:1946"),
    )
    assert target.request_url.startswith("http://[2606:2800:220:1:248:1893:25c8:1946]:8080/")
    assert target.host_header.endswith(":8080")
    assert target.request_extensions() == {}


def test_target_is_frozen():
    t = Target(
        url="http://x/",
        scheme="http",
        host="x",
        port=80,
        pinned_ip="93.184.216.34",
        path_query="/",
    )
    with pytest.raises(FrozenInstanceError):
        t.host = "y"  # type: ignore[misc]


# --- review fixes (2026-10-14) ---------------------------------------------


@pytest.mark.parametrize("url", ["http://[::1/x", "http://[2001:db8::1/a", "http://[/"])
async def test_malformed_url_raises_web_url_error(url):
    """urlsplit's ValueError must map onto WebUrlError (correctable class),
    not escape as an untyped fail-fast error (review F1)."""
    with pytest.raises(WebUrlError):
        await validate_target(url, resolver=resolver_for("93.184.216.34"))


async def test_overlong_hostname_label_is_transient():
    """getaddrinfo raises UnicodeError (a ValueError, not OSError) for an
    over-long label — must classify as resolution failure (review F3)."""
    async def _resolve(host: str, port: int) -> list[str]:
        raise UnicodeError("label empty or too long")

    with pytest.raises(WebResolveError):
        await validate_target("http://a.test/", resolver=_resolve)


async def test_private_refusal_omits_resolved_address():
    """The refusal names the class, not the address (review F7): the text
    reaches the model + browser UI."""
    with pytest.raises(WebUrlError) as excinfo:
        await validate_target(
            "http://internal.test/", resolver=resolver_for("10.11.12.13")
        )
    assert "non-public" in str(excinfo.value)
    assert "10.11.12.13" not in str(excinfo.value)


async def test_ensure_public_address_refuses_denied_classes():
    for address in ("127.0.0.1", "10.1.2.3", "169.254.169.254", "::1", "0.0.0.0"):
        with pytest.raises(WebUrlError):
            await ensure_public_address(
                "http://llm.test/v1", resolver=resolver_for(address)
            )


async def test_ensure_public_address_allows_public_hosts():
    await ensure_public_address(
        "http://llm.test/v1", resolver=resolver_for("93.184.216.34")
    )


async def test_ensure_public_address_allow_private_permits_denied_classes():
    await ensure_public_address(
        "http://127.0.0.1:8000/v1",
        allow_private=True,
        resolver=resolver_for("127.0.0.1"),
    )


async def test_ensure_public_address_dns_failure_is_transient():
    def failing(host: str, port: int):
        async def _resolve(host: str, port: int) -> list[str]:
            raise socket.gaierror(-2, "Name or service not known")

        return _resolve(host, port)

    with pytest.raises(WebResolveError):
        await ensure_public_address("http://missing.test/v1", resolver=failing)


async def test_ensure_public_address_has_no_port_restriction():
    await ensure_public_address(
        "http://llm.test:8080/v1", resolver=resolver_for("93.184.216.34")
    )


async def test_ensure_public_address_refusal_omits_the_resolved_ip():
    with pytest.raises(WebUrlError) as excinfo:
        await ensure_public_address(
            "http://internal.test/v1", resolver=resolver_for("10.11.12.13")
        )
    assert "non-public" in str(excinfo.value)
    assert "10.11.12.13" not in str(excinfo.value)
