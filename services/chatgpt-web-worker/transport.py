# SPDX-License-Identifier: AGPL-3.0-only
"""Bounded HTTP with fixed TLS fingerprint and isolated, DNS-pinned asset requests."""
import asyncio
import ipaddress
import json
import socket
from contextlib import asynccontextmanager
from urllib.parse import urlsplit

from curl_cffi import CurlOpt, ffi, lib
from curl_cffi.requests import AsyncSession

from contract import SafeError

ASSET_HOSTS = frozenset({"chatgpt.com", "files.oaiusercontent.com", "oaiusercontent.com",
                         "oaidalleapiprodscus.blob.core.windows.net",
                         "prod-files-secure.s3.us-west-2.amazonaws.com"})


def authenticated_asset_url(url):
    """Only the authenticated same-origin image endpoint receives account ATs."""
    try:
        p = urlsplit(url)
        return (p.scheme == "https" and p.hostname == "chatgpt.com" and p.port in {None, 443}
                and p.path == "/backend-api/estuary/content" and not p.username and not p.password
                and not p.fragment and "\\" not in url and not any(ord(c) < 33 for c in url))
    except (TypeError, ValueError):
        return False


async def public_destination(url):
    try:
        part = urlsplit(url)
        if (part.scheme != "https" or part.hostname not in ASSET_HOSTS or part.port not in {None, 443}
                or part.username or part.password or part.fragment or "\\" in url
                or any(ord(c) < 33 for c in url) or len(url) > 16384):
            raise ValueError()
        answers = await asyncio.wait_for(asyncio.get_running_loop().getaddrinfo(
            part.hostname, 443, type=socket.SOCK_STREAM), 5)
        addresses = sorted({answer[4][0] for answer in answers})
        if not addresses or any(not ipaddress.ip_address(address).is_global or
                                ipaddress.ip_address(address).is_multicast or
                                ipaddress.ip_address(address).is_reserved for address in addresses):
            raise ValueError()
        return part.hostname, addresses
    except Exception:
        raise SafeError("image_download_failed") from None


async def read_bounded(response, limit):
    data = bytearray()
    async for chunk in response.aiter_content():
        data.extend(chunk)
        if len(data) > limit:
            raise SafeError("upstream_error")
    return bytes(data)


def check_status(response, asset=False):
    status = response.status_code
    if 200 <= status < 300:
        return
    if asset:
        raise SafeError("image_download_failed", upstream_status=status)
    if status == 401:
        raise SafeError("auth_required", 401, upstream_status=status)
    if status == 403:
        raise SafeError("environment_challenge", 403, upstream_status=status)
    if status == 429:
        raise SafeError("quota_exhausted", 429, upstream_status=status)
    raise SafeError("upstream_error", upstream_status=status)


class CurlTransport:
    def __init__(self, account):
        self.proxy = account.raw.get("proxy_url") or ""
        self.options = {CurlOpt.PROXY: self.proxy, CurlOpt.NOPROXY: "",
                        CurlOpt.SSL_VERIFYPEER: 1, CurlOpt.SSL_VERIFYHOST: 2,
                        CurlOpt.PROXY_SSL_VERIFYPEER: 1, CurlOpt.PROXY_SSL_VERIFYHOST: 2}
        self.session = AsyncSession(impersonate="chrome110", verify=True, trust_env=False,
                                   proxy=self.proxy, max_clients=2, curl_options=dict(self.options))

    @asynccontextmanager
    async def open(self, method, url, *, headers, timeout, asset=False, **kwargs):
        session, response, connect_to = self.session, None, ffi.NULL
        try:
            if asset:
                host, addresses = await public_destination(url)
                # Never propagate account credentials to storage origins or PUTs.
                allowed_auth = method == "GET" and authenticated_asset_url(url)
                headers = {k: v for k, v in headers.items()
                           if k.lower() not in {"cookie", "proxy-authorization"}
                           and (k.lower() != "authorization" or allowed_auth)}
                address = addresses[0]
                target = f"[{address}]" if ":" in address else address
                # curl_cffi 0.15 handles RESOLVE lists but CONNECT_TO needs a
                # native slist. Keep it alive until the isolated session closes.
                connect_to = lib.curl_slist_append(ffi.NULL, f"{host}:443:{target}:443".encode())
                if connect_to == ffi.NULL:
                    raise SafeError("image_download_failed")
                session = AsyncSession(impersonate="chrome110", verify=True, trust_env=False,
                                       proxy=self.proxy,
                                       curl_options=self.options | {CurlOpt.CONNECT_TO: connect_to,
                                                                   CurlOpt.RESOLVE: [f"{host}:443:{target}"]})
            response = await session.request(method, url, headers=headers, timeout=timeout,
                                             allow_redirects=False, stream=True, **kwargs)
            check_status(response, asset)
            yield response
        finally:
            try:
                if response is not None:
                    await response.aclose()
            finally:
                try:
                    if asset and session is not self.session:
                        await session.close()
                finally:
                    if connect_to != ffi.NULL:
                        lib.curl_slist_free_all(connect_to)

    async def close(self):
        await self.session.close()


async def json_response(response, limit=4 * 1024 * 1024):
    try:
        value = json.loads(await read_bounded(response, limit))
        if not isinstance(value, dict):
            raise ValueError()
        return value
    except SafeError:
        raise
    except Exception:
        raise SafeError("upstream_error") from None
