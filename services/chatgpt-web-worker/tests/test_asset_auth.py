# SPDX-License-Identifier: AGPL-3.0-only
import asyncio
from types import SimpleNamespace
from urllib.parse import urlsplit

import pytest
from curl_cffi import Curl, CurlOpt, ffi, lib

import transport
from backend import Backend
from contract import SafeError
from conversation import Conversation
from test_worker import Script, account
from transport import CurlTransport, authenticated_asset_url


@pytest.mark.parametrize('url,allowed', [
    ('https://chatgpt.com/backend-api/estuary/content?id=synthetic', True),
    ('https://chatgpt.com:443/backend-api/estuary/content?id=synthetic', True),
    ('https://files.oaiusercontent.com/file', False),
    ('https://chatgpt.com.evil.invalid/backend-api/estuary/content', False),
    ('https://user:pass@chatgpt.com/backend-api/estuary/content', False),
    ('http://chatgpt.com/backend-api/estuary/content', False),
    ('https://chatgpt.com:444/backend-api/estuary/content', False),
    ('https://chatgpt.com/backend-api/estuary/content/../other', False),
    ('https://chatgpt.com/backend-api/estuary/content#fragment', False),
    ('https://chatgpt.com/other', False),
])
def test_asset_auth_origin_boundary(url, allowed):
    assert authenticated_asset_url(url) is allowed


@pytest.mark.parametrize('url,auth', [
    ('https://chatgpt.com/backend-api/estuary/content?id=synthetic', True),
    ('https://files.oaiusercontent.com/output?sig=synthetic', False),
])
async def test_backend_download_scopes_bearer(url, auth):
    script = Script()
    script.responses['https://chatgpt.com/backend-api/files/file_output/download'] = {'download_url': url}
    b = script.backend(account())
    try:
        image = await b.download('file_output', Conversation('conv-1'))
        assert image['width'] > 0
        headers = next(call[2]['headers'] for call in script.calls if call[1] == url)
        assert bool(headers.get('Authorization')) is auth
        assert 'Cookie' not in headers
    finally:
        await b.transport.close()


def test_real_curl_accepts_owned_connect_to_slist():
    # No request/network: exercise the actual pinned dependency, not a mock
    # that would mistakenly accept the previous invalid Python list.
    links = lib.curl_slist_append(ffi.NULL, b'chatgpt.com:443:8.8.8.8:443')
    assert links != ffi.NULL
    c = Curl()
    try:
        c.setopt(CurlOpt.CONNECT_TO, links)
        c.setopt(CurlOpt.RESOLVE, ['chatgpt.com:443:8.8.8.8'])
        c.setopt(CurlOpt.SSL_VERIFYPEER, 1)
        c.setopt(CurlOpt.SSL_VERIFYHOST, 2)
    finally:
        c.close()
        lib.curl_slist_free_all(links)


@pytest.mark.parametrize('method,url,auth', [
    ('GET','https://chatgpt.com/backend-api/estuary/content?id=synthetic', True),
    ('GET','https://files.oaiusercontent.com/output?sig=synthetic', False),
    ('PUT','https://files.oaiusercontent.com/input?sig=synthetic', False),
    ('GET','https://chatgpt.com/other', False),
])
async def test_transport_defense_and_native_pin_lifetime(monkeypatch, method, url, auth):
    records = []
    freed = []
    real_lib = transport.lib
    def free(p):
        freed.append(p)
        real_lib.curl_slist_free_all(p)
    monkeypatch.setattr(transport, 'lib', SimpleNamespace(curl_slist_append=real_lib.curl_slist_append, curl_slist_free_all=free))
    class Response:
        status_code = 200
        async def aclose(self): pass
    class Session:
        def __init__(self, **kw): self.kw = kw; self.closed = False; records.append(self)
        async def request(self, m, u, **kw):
            assert m == method and u == url
            h = {k.lower():v for k,v in kw['headers'].items()}
            assert bool(h.get('authorization')) is auth
            assert 'cookie' not in h and 'proxy-authorization' not in h
            assert h['content-type'] == 'image/png'
            assert kw['allow_redirects'] is False
            ptr = self.kw['curl_options'][CurlOpt.CONNECT_TO]
            assert ffi.string(ptr.data).decode() == f'{urlsplit(url).hostname}:443:8.8.8.8:443'
            assert not freed
            assert self.kw['proxy'] == account().raw['proxy_url']
            assert self.kw['verify'] is True and self.kw['trust_env'] is False
            return Response()
        async def close(self): self.closed = True
    async def resolve(u, m): return urlsplit(u).hostname, ['8.8.8.8']
    monkeypatch.setattr(transport, 'AsyncSession', Session)
    monkeypatch.setattr(transport, 'public_destination', resolve)
    client = CurlTransport(account())
    try:
        async with client.open(method,url,headers={'authorization':'Bearer secret','Cookie':'secret','Proxy-Authorization':'secret','Content-Type':'image/png'},timeout=10,asset=True): pass
        assert records[-1].closed and len(freed) == 1
    finally:
        await client.close()


async def test_failed_asset_session_construction_frees_pin(monkeypatch):
    freed = []
    real_lib = transport.lib
    def free(p): freed.append(p);real_lib.curl_slist_free_all(p)
    monkeypatch.setattr(transport,'lib',SimpleNamespace(curl_slist_append=real_lib.curl_slist_append,curl_slist_free_all=free))
    class Session:
        def __init__(self,**kw):
            if CurlOpt.CONNECT_TO in kw['curl_options']: raise RuntimeError('synthetic construction failure')
        async def close(self): pass
    async def resolve(u, m):return 'chatgpt.com',['8.8.8.8']
    monkeypatch.setattr(transport,'AsyncSession',Session)
    monkeypatch.setattr(transport,'public_destination',resolve)
    client=CurlTransport(account())
    try:
        with pytest.raises(RuntimeError):
            async with client.open('GET','https://chatgpt.com/backend-api/estuary/content',headers={},timeout=10,asset=True):pass
        assert len(freed)==1
    finally:await client.close()
