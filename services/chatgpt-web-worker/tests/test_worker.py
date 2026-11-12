# SPDX-License-Identifier: AGPL-3.0-only
"""No upstream network is permitted in this suite; HTTP transport is fully scripted."""
import asyncio
import base64
import copy
import io
import json
import socket
from contextlib import asynccontextmanager

import httpx
import pytest
from PIL import Image

from app import create_app
from backend import Backend
from contract import Account, SafeError
from conversation import Conversation, sse_events
from executor import Executor
from ledger import Ledger
from transport import CurlTransport, check_status, public_destination


@pytest.fixture(autouse=True)
def deny_network(monkeypatch):
    import sys
    original = socket.socket.connect
    def denied(*args, **kwargs):
        raise AssertionError("Tests must not make network connections")
    def guarded_connect(sock, address):
        caller = sys._getframe(1)
        # Windows asyncio uses the stdlib loopback socketpair for its wakeup pipe.
        if caller.f_code.co_name in {"socketpair", "_fallback_socketpair"} and caller.f_code.co_filename.endswith("socket.py"):
            return original(sock, address)
        return denied()
    monkeypatch.setattr(socket.socket, "connect", guarded_connect)
    monkeypatch.setattr(socket, "getaddrinfo", denied)
    from curl_cffi.requests import AsyncSession
    monkeypatch.setattr(AsyncSession, "request", denied)


def png():
    output = io.BytesIO()
    Image.new("RGB", (8, 6), "green").save(output, format="PNG")
    return output.getvalue()


def account(**changes):
    import hashlib
    raw = {"account_id": 123, "access_token": "mock-access-token", "chatgpt_account_id": "mock-account",
           "credential_revision": hashlib.sha256(b"mock-access-token").hexdigest(), "environment_revision": 1,
           "proxy_url": "http://mock-user:mock-pass@proxy.example:8080",
           "environment": {"device_id": "device-id", "session_id": "session-id"}, "execution_id": "exec-1",
           "operation": "generate", "model": "gpt-image-2", "prompt": "private test prompt", "n": 1,
           "size": "auto", "quality": "auto", "output_format": "png", "timeout_seconds": 10}
    raw.update(changes)
    return Account.parse(raw, True)


def message(role="tool", pointer="file-service://file_out", **changes):
    value = {"author": {"role": role}, "content": {"content_type": "multimodal_text", "parts": [
             {"content_type": "image_asset_pointer", "asset_pointer": pointer}]},
             "metadata": {"async_task_type": "image_gen"}}
    value.update(changes)
    return value


def sse(*events):
    return b"".join(b"data: " + json.dumps(e).encode() + b"\n\n" for e in events) + b"data: [DONE]\n\n"


class Response:
    def __init__(self, body, status=200):
        self.body = json.dumps(body).encode() if isinstance(body, dict) else body
        self.status_code = status
        self.closed = False

    async def aiter_content(self):
        for i in range(0, len(self.body), 31):
            yield self.body[i:i + 31]

    async def aclose(self):
        self.closed = True


class Script:
    def __init__(self, stream=None):
        self.calls, self.closed = [], False
        self.stream = stream if stream is not None else sse({"conversation_id": "conv-1", "message": message()})
        self.fail_submit = False
        self.fail_download = False
        self.quota = 2
        self.challenge = None
        self.conversation = {"mapping": {"m": {"message": message()}}}
        self.tasks = {"tasks": []}
        self.statuses = {}
        self.submission_assert = None

    @asynccontextmanager
    async def open(self, method, url, **kwargs):
        self.calls.append((method, url, kwargs))
        if kwargs.get("asset"):
            assert "Authorization" not in kwargs["headers"]
            assert "Cookie" not in kwargs["headers"]
            if method == "GET" and self.fail_download:
                raise RuntimeError("sensitive signed URL and token must not leak")
            body = png() if method == "GET" else b""
        elif url.endswith("/f/conversation"):
            assert method == "POST"
            if self.submission_assert:
                self.submission_assert()
            if self.fail_submit:
                raise RuntimeError("mock-access-token https://secret.example/?signed=secret")
            body = self.stream
        elif url.endswith("/chat-requirements/prepare"):
            body = {"prepare_token": "prepare-mock", **(self.challenge or {})}
        elif url.endswith("/chat-requirements/finalize"):
            body = {"token": "requirements-mock"}
        elif url.endswith("/f/conversation/prepare"):
            body = {"conduit_token": "conduit-mock"}
        elif url.endswith("/backend-api/files"):
            body = {"file_id": "file_input", "upload_url": "https://files.oaiusercontent.com/input?sig=private"}
        elif url.endswith("/uploaded"):
            body = {}
        elif url.endswith("/download"):
            body = {"download_url": "https://files.oaiusercontent.com/output?sig=private"}
        elif "/backend-api/conversation/conv-" in url:
            body = self.conversation
        elif url.endswith("/tasks"):
            body = self.tasks
        elif url.endswith("/me"):
            body = {"id": "mock-user"}
        elif url.endswith("/conversation/init"):
            body = {"plan_type": "plus", "limits_progress": [] if self.quota is None else [
                    {"feature_name": "image_gen", "remaining": self.quota, "reset_after": "2026-11-12T12:00:00Z"}]}
        elif url == "https://chatgpt.com/":
            body = b'<html data-build="test"><script src="/sdk.js"></script></html>'
        else:
            raise AssertionError("Unexpected mock HTTP endpoint")
        response = Response(body, self.statuses.get(url, 200))
        check_status(response, kwargs.get("asset", False))
        try:
            yield response
        finally:
            await response.aclose()

    async def close(self):
        self.closed = True

    def backend(self, acc):
        return Backend(acc, lambda _: self)

    def submits(self):
        return [call for call in self.calls if call[1].endswith("/f/conversation")]


async def execute(tmp_path, script, acc=None):
    ledger = Ledger(tmp_path)
    result = [event async for event in Executor(ledger, script.backend).events(acc or account())]
    return result, ledger


async def test_generate_fence_success_and_cache_replay(tmp_path):
    script = Script()
    script.submission_assert = lambda: assert_fenced(Ledger(tmp_path))
    events, ledger = await execute(tmp_path, script)
    assert [e["stage"] for e in events if e["type"] == "stage"] == ["prepared", "submitting", "submitted", "generated", "downloading"]
    assert events[-1] == {"type": "completed", "usage_known": False}
    assert base64.b64decode(events[-2]["image"]["b64_json"]) == png()
    assert events[-2]["image"]["width"] == 8
    before = len(script.calls)
    replay = [e async for e in Executor(Ledger(tmp_path), script.backend).events(account())]
    assert len(script.calls) == before and replay[-1]["type"] == "completed"
    assert len(script.submits()) == 1
    state = json.dumps(ledger.public("exec-1")) + ledger.db.read_bytes().decode(errors="ignore")
    for secret in ("mock-access-token", "mock-pass", "private test prompt", "sig=private", base64.b64encode(png()).decode()):
        assert secret not in state


def assert_fenced(ledger):
    assert ledger.get("exec-1")["stage"] == "submitting"


async def test_edit_upload_and_no_input_output_confusion(tmp_path):
    script = Script(sse({"conversation_id": "conv-1", "message": message("user", "file-service://file_input")},
                        {"message": message(pointer="file-service://file_input")}, {"message": message()}))
    acc = account(operation="edit", images=[{"b64_json": base64.b64encode(png()).decode(), "mime_type": "image/png"}])
    events, ledger = await execute(tmp_path, script, acc)
    assert events[-1]["type"] == "completed"
    assert ledger.get("exec-1")["assets"] == ["file_out"]
    assert any(c[0] == "PUT" and c[2]["data"] == png() for c in script.calls)
    submitted = script.submits()[0][2]["json"]
    assert submitted["model"] == "gpt-5-3"
    assert submitted["messages"][0]["metadata"]["attachments"][0]["id"] == "file_input"


@pytest.mark.parametrize("fmt,mime", [("png", "image/png"), ("jpeg", "image/jpeg"), ("webp", "image/webp")])
async def test_output_format(tmp_path, fmt, mime):
    events, _ = await execute(tmp_path, Script(), account(output_format=fmt, size="16:9"))
    image = next(e["image"] for e in events if e["type"] == "image")
    assert image["mime_type"] == mime
    with Image.open(io.BytesIO(base64.b64decode(image["b64_json"]))) as decoded:
        assert decoded.size == (8, 6)


@pytest.mark.parametrize("quota,status", [(2, "ready"), (0, "quota_exhausted"), (None, "unverified"), ("5", "unverified")])
async def test_account_check_positive_evidence_only(quota, status):
    script = Script()
    script.quota = quota
    result = await script.backend(account()).check()
    assert result["status"] == status
    assert result["capabilities"] == (["generate"] if status == "ready" else [])
    assert not script.submits()
    assert all(not c[1].endswith("/f/conversation/prepare") for c in script.calls)
    assert all("settings" not in c[1] for c in script.calls)


@pytest.mark.parametrize("status,expected", [(401, "auth_required"), (403, "challenge"), (429, "quota_exhausted")])
async def test_account_auth_challenge_quota(status, expected):
    script = Script()
    script.statuses["https://chatgpt.com/backend-api/me"] = status
    result = await script.backend(account()).check()
    assert result["status"] == expected and not script.submits()


@pytest.mark.parametrize("challenge", ["arkose", "turnstile"])
async def test_unsupported_challenge_fails_before_generation(tmp_path, challenge):
    script = Script()
    script.challenge = {challenge: {"required": True, "dx": "not-executed"}}
    events, _ = await execute(tmp_path, script)
    assert events[-1]["code"] == "environment_challenge" and not script.submits()


def test_json_patches_and_trust_boundary():
    state = Conversation(inputs=["file_input"])
    state.feed({"o": "add", "p": "", "v": {"conversation_id": "conv-1", "message": message(pointer="sediment://asset_out")}})
    assert state.assets == ["sediment://asset_out"]
    state.feed({"message": message("user", "sediment://not_output")})
    state.feed({"o": "patch", "v": [{"p": "/message/content/parts/0/asset_pointer", "o": "replace", "v": "sediment://still_input"}]})
    assert state.assets == ["sediment://asset_out"]
    state.feed({"message": message(pointer="file-service://file_input")})
    state.feed({"o": "patch", "v": [{"p": "/message/content/parts/0/asset_pointer", "o": "replace", "v": "file-service://file_new"}]})
    assert state.assets == ["sediment://asset_out", "file_new"]
    state.feed({"message": message("assistant", "file-service://assistant_not_output")})
    assert "assistant_not_output" not in state.assets


async def test_sse_crlf_multi_chunk_and_incomplete():
    response = Response(b': ping\r\ndata: {"conversation_id":\r\ndata: "conv-1"}\r\n\r\ndata: [DONE]\r\n\r\n')
    assert [e async for e in sse_events(response)] == [{"conversation_id": "conv-1"}]
    with pytest.raises(SafeError):
        _ = [e async for e in sse_events(Response(b'data: {"v":'))]


async def test_unknown_submission_restart_never_resubmits(tmp_path):
    script = Script()
    script.fail_submit = True
    events, ledger = await execute(tmp_path, script)
    assert events[-1]["code"] == "submission_unknown" and events[-1]["result_unknown"]
    script.fail_submit = False
    replay = [e async for e in Executor(Ledger(tmp_path), script.backend).events(account())]
    assert replay[-1]["code"] == "submission_unknown" and len(script.submits()) == 1
    assert ledger.get("exec-1")["stage"] == "result_unknown"
    assert "mock-access-token" not in json.dumps(events)


async def test_crash_at_submitting_without_ids(tmp_path):
    ledger = Ledger(tmp_path)
    ledger.claim(account())
    ledger.update("exec-1", stage="prepared")
    ledger.fence("exec-1")
    script = Script()
    events = [e async for e in Executor(Ledger(tmp_path), script.backend).events(account())]
    assert events[-1]["code"] == "submission_unknown" and not script.calls


async def test_restart_known_conversation_get_only(tmp_path):
    ledger = Ledger(tmp_path)
    ledger.claim(account())
    ledger.update("exec-1", stage="submitting", conversation_id="conv-1")
    script = Script()
    events = [e async for e in Executor(Ledger(tmp_path), script.backend).events(account())]
    assert events[-1]["type"] == "completed"
    assert script.calls and all(c[0] == "GET" for c in script.calls)


async def test_restart_known_task_get_only_and_not_unrelated_tasks(tmp_path):
    ledger = Ledger(tmp_path)
    ledger.claim(account())
    ledger.update("exec-1", stage="submitted", task_id="task-1")
    script = Script()
    script.tasks = {"tasks": [{"task_id": "other", "image_gen_message": message(pointer="file-service://not_ours")},
                              {"task_id": "task-1", "conversation_id": "conv-1", "image_gen_message": message()}]}
    events = [e async for e in Executor(ledger, script.backend).events(account())]
    assert events[-1]["type"] == "completed"
    assert ledger.get("exec-1")["assets"] == ["file_out"]
    assert all(c[0] == "GET" for c in script.calls)


async def test_download_failure_resume_download_only(tmp_path):
    script = Script()
    script.fail_download = True
    events, ledger = await execute(tmp_path, script)
    assert events[-1]["code"] == "image_download_failed"
    script.fail_download = False
    script.calls.clear()
    events = [e async for e in Executor(ledger, script.backend).events(account())]
    assert events[-1]["type"] == "completed"
    assert all(c[0] == "GET" for c in script.calls)
    assert all("/conversation/conv" not in c[1] for c in script.calls)


async def test_active_duplicate_cannot_submit(tmp_path):
    ledger = Ledger(tmp_path)
    script = Script()
    first = Executor(ledger, script.backend).events(account())
    assert (await anext(first))["stage"] == "prepared"
    second = [e async for e in Executor(Ledger(tmp_path), script.backend).events(account())]
    assert second[-1]["code"] == "execution_conflict" and not script.submits()
    remaining = [e async for e in first]
    assert remaining[-1]["type"] == "completed" and len(script.submits()) == 1


@pytest.mark.parametrize("change", [{"prompt": "changed"}, {"environment_revision": 2},
    {"environment": {"device_id": "changed", "session_id": "session-id"}}, {"proxy_url": ""},
    {"account_id": 456}, {"chatgpt_account_id": "other"}, {"output_format": "webp"}])
async def test_immutable_binding_conflict(tmp_path, change):
    script = Script()
    await execute(tmp_path, script)
    count = len(script.calls)
    result = [e async for e in Executor(Ledger(tmp_path), script.backend).events(account(**change))]
    assert result[-1]["code"] == "execution_conflict" and len(script.calls) == count


async def test_credential_revision_conflict(tmp_path):
    import hashlib
    script = Script()
    await execute(tmp_path, script)
    other = account(access_token="other-token", credential_revision=hashlib.sha256(b"other-token").hexdigest())
    result = [e async for e in Executor(Ledger(tmp_path), script.backend).events(other)]
    assert result[-1]["code"] == "execution_conflict"
    with pytest.raises(SafeError):
        account(access_token="other-token")


async def test_cancel_after_fence_preserves_unknown(tmp_path):
    script, ledger = Script(), Ledger(tmp_path)
    events = Executor(ledger, script.backend).events(account())
    assert (await anext(events))["stage"] == "prepared"
    assert (await anext(events))["stage"] == "submitting"
    await events.aclose()
    replay = [e async for e in Executor(Ledger(tmp_path), script.backend).events(account())]
    assert replay[-1]["code"] == "submission_unknown" and not script.submits()


async def test_partial_image_then_error_is_not_completed(tmp_path):
    script = Script(sse({"conversation_id": "conv-1", "message": message()}, {"error": {"code": "content_policy_violation"}}))
    events, ledger = await execute(tmp_path, script)
    assert any(e["type"] == "image" for e in events)
    assert events[-1]["code"] == "content_policy_violation"
    assert not any(e["type"] == "completed" for e in events)
    assert ledger.get("exec-1")["cache"]


async def test_pending_task_returns_bounded_safe_error(tmp_path):
    script = Script(sse({"conversation_id": "conv-1"}))
    script.conversation = {"mapping": {}}
    events, _ = await execute(tmp_path, script)
    assert events[-1]["code"] == "image_poll_timeout"
    assert len(script.submits()) == 1
    assert sum(c[1].endswith("/tasks") for c in script.calls) == 1


@pytest.mark.parametrize("url", ["http://files.oaiusercontent.com/x", "https://127.0.0.1/x", "https://evil.example/x",
    "https://files.oaiusercontent.com.evil.example/x", "https://files.oaiusercontent.com:444/x",
    "https://user:pass@files.oaiusercontent.com/x", "https://files.oaiusercontent.com/x#fragment",
    "https://files.oaiusercontent.com\\@evil.example/x", "file:///etc/passwd"])
async def test_asset_url_ssrf_rejected(url):
    with pytest.raises(SafeError) as error:
        await public_destination(url)
    assert error.value.code == "image_download_failed"


@pytest.mark.parametrize("address", ["127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fc00::1", "::ffff:127.0.0.1"])
async def test_private_dns_rejected(monkeypatch, address):
    async def lookup(*args, **kwargs):
        return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (address, 443))]
    monkeypatch.setattr(asyncio.get_running_loop(), "getaddrinfo", lookup)
    with pytest.raises(SafeError):
        await public_destination("https://files.oaiusercontent.com/x")


async def test_asset_dns_pinning_tls_proxy_no_auth_and_redirect_rejection(monkeypatch):
    import transport
    created = []
    class Session:
        def __init__(self, **kwargs):
            self.options = kwargs
            self.closed = False
            created.append(self)
        async def request(self, method, url, **kwargs):
            self.request_options = kwargs
            return Response(b"", status=302)
        async def close(self):
            self.closed = True
    async def lookup(url):
        return "files.oaiusercontent.com", ["8.8.8.8"]
    monkeypatch.setattr(transport, "AsyncSession", Session)
    monkeypatch.setattr(transport, "public_destination", lookup)
    client = CurlTransport(account())
    with pytest.raises(SafeError):
        async with client.open("GET", "https://files.oaiusercontent.com/x?secret", headers={}, timeout=10, asset=True):
            pytest.fail("Redirect must not be followed")
    assert created[-1].options["verify"] is True
    assert created[-1].options["trust_env"] is False
    assert created[-1].options["proxy"] == account().raw["proxy_url"]
    assert any("8.8.8.8" in str(value) for value in created[-1].options["curl_options"].values())
    assert created[-1].request_options["headers"] == {}
    assert created[-1].request_options["allow_redirects"] is False and created[-1].closed
    await client.close()


async def test_http_contract_auth_health_and_ndjson(tmp_path):
    script = Script()
    app = create_app("worker-secret", tmp_path, script.backend)
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://worker") as client:
        health = await client.get("/health")
        assert health.json() == {"status": "ok", "protocol_version": "1"}
        for path in ("/v1/accounts/check", "/v1/images/execute", "/v1/executions/exec-1", "/unknown"):
            assert (await client.get(path)).status_code == 401
        headers = {"Authorization": "Bearer worker-secret"}
        response = await client.post("/v1/images/execute", json=account().raw, headers=headers)
        assert response.status_code == 200 and response.headers["content-type"].startswith("application/x-ndjson")
        events = [json.loads(line) for line in response.text.splitlines()]
        assert events[-1]["type"] == "completed"
        state = (await client.get("/v1/executions/exec-1", headers=headers)).json()
        assert state["result_available"] is True and state["account_id"] == 123
        bad = await client.post("/v1/images/execute", content=b'{"access_token":"never-echo-this"}', headers=headers)
        assert bad.status_code == 400 and "never-echo-this" not in bad.text


def test_missing_secret_fails_closed(tmp_path, monkeypatch):
    monkeypatch.delenv("CHATGPT_WEB_WORKER_SECRET", raising=False)
    with pytest.raises(RuntimeError):
        create_app(state_dir=tmp_path)


@pytest.mark.parametrize("changes", [{"model": "other"}, {"n": 2}, {"quality": "high"}, {"size": "1024x1024"},
    {"output_format": "gif"}, {"operation": "edit", "images": []}, {"images": [{"url": "https://evil.example"}]},
    {"headers": {"Authorization": "bad"}}, {"upstream_url": "http://localhost"}, {"timeout_seconds": 601},
    {"environment_revision": True}, {"execution_id": "../../bad"}])
def test_unsupported_inputs_rejected(changes):
    with pytest.raises(SafeError):
        account(**changes)


async def test_retention_keeps_fence_and_resumes_assets(tmp_path):
    script = Script()
    await execute(tmp_path, script)
    ledger = Ledger(tmp_path, retention=0)
    ledger.prune()
    assert ledger.get("exec-1")["cache"] == [] and ledger.get("exec-1")["assets"]
    script.calls.clear()
    events = [e async for e in Executor(ledger, script.backend).events(account())]
    assert events[-1]["type"] == "completed" and all(c[0] == "GET" for c in script.calls)


async def test_capacity_rejected_before_generation(tmp_path):
    ledger, script = Ledger(tmp_path, max_records=0), Script()
    events = [e async for e in Executor(ledger, script.backend).events(account())]
    assert events[-1]["code"] == "capacity_exceeded" and not script.calls


async def test_partial_terminal_error_is_preserved_on_replay(tmp_path):
    script = Script(sse({"conversation_id": "conv-1", "message": message()}, {"error": {"code": "content_policy_violation"}}))
    _, ledger = await execute(tmp_path, script)
    count = len(script.submits())
    events = [e async for e in Executor(ledger, script.backend).events(account())]
    assert events[-1]["code"] == "content_policy_violation"
    assert not any(e["type"] == "completed" for e in events)
    assert len(script.submits()) == count


async def test_truncated_stream_after_asset_preserves_partial(tmp_path):
    body = sse({"conversation_id": "conv-1", "message": message()}).replace(b"data: [DONE]\n\n", b'data: {"truncated":')
    events, ledger = await execute(tmp_path, Script(body))
    assert any(e["type"] == "image" for e in events)
    assert events[-1]["code"] == "image_stream_interrupted"
    assert ledger.get("exec-1")["cache"]


async def test_timeout_after_submit_preserves_unknown(tmp_path):
    class SlowScript(Script):
        @asynccontextmanager
        async def open(self, method, url, **kwargs):
            if url.endswith("/f/conversation"):
                self.calls.append((method, url, kwargs))
                await asyncio.Event().wait()
            async with super().open(method, url, **kwargs) as response:
                yield response
    acc = account()
    acc.raw["timeout_seconds"] = 0.05  # Test clock budget only; public validation still enforces 10..600.
    events, ledger = await execute(tmp_path, SlowScript(), acc)
    assert events[-1]["code"] == "submission_unknown" and events[-1]["result_unknown"]
    assert ledger.get("exec-1")["stage"] == "result_unknown"


def test_cache_orphans_count_toward_capacity(tmp_path):
    ledger = Ledger(tmp_path, max_cache=20)
    ledger.claim(account())
    (ledger.cache / "orphan.json").write_bytes(b"x" * 20)
    with pytest.raises(SafeError) as error:
        ledger.store("exec-1", [{"asset_id": "out", "b64_json": "real-cached-test-data"}])
    assert error.value.code == "capacity_exceeded"


async def test_lock_files_not_created_for_rejected_new_ids(tmp_path):
    ledger = Ledger(tmp_path, max_records=0)
    _ = [e async for e in Executor(ledger, Script().backend).events(account())]
    assert list(ledger.locks.iterdir()) == []


async def test_real_curl_session_can_construct_without_network():
    client = CurlTransport(account(proxy_url=""))
    from curl_cffi import CurlOpt
    assert client.options[CurlOpt.PROXY] == ""
    assert client.options[CurlOpt.NOPROXY] == ""
    assert client.options[CurlOpt.SSL_VERIFYPEER] == 1
    await client.close()


def test_pow_and_requirements_tokens_are_bounded():
    import time
    from proof import Resources, proof_token, requirements_token
    resources = Resources('<html data-build="fixed"><script src="/c/fixed/_sdk.js"></script></html>')
    assert requirements_token("mock-UA", resources).startswith("gAAAAAC")
    assert proof_token({"required": True, "seed": "test", "difficulty": "ff"}, "mock-UA", resources, time.monotonic() + 1).startswith("gAAAAAB")
    with pytest.raises(SafeError) as error:
        proof_token({"required": True, "seed": "test", "difficulty": "not-hex"}, "mock-UA", resources, time.monotonic() + 1)
    assert error.value.code == "environment_challenge"


async def test_missing_output_cache_redownloads_without_resubmit(tmp_path):
    script = Script()
    _, ledger = await execute(tmp_path, script)
    for path in ledger.cache.iterdir():
        path.unlink()
    script.calls.clear()
    events = [e async for e in Executor(ledger, script.backend).events(account())]
    assert events[-1]["type"] == "completed" and all(c[0] == "GET" for c in script.calls)


def test_unknown_get_projection_distinguishes_active_owner(tmp_path):
    ledger = Ledger(tmp_path)
    ledger.claim(account())
    ledger.update("exec-1", stage="prepared")
    ledger.fence("exec-1")
    with ledger.owner("exec-1"):
        assert ledger.public("exec-1")["stage"] == "submitting"
    assert ledger.public("exec-1")["stage"] == "result_unknown"
    assert ledger.get("exec-1")["stage"] == "submitting"  # GET projection never clears/rewrites fence.


async def test_cancelled_known_conversation_can_resume_get_only(tmp_path):
    ledger = Ledger(tmp_path)
    ledger.claim(account())
    ledger.update("exec-1", stage="submitted", conversation_id="conv-1", error="image_stream_interrupted")
    script = Script()
    events = [e async for e in Executor(ledger, script.backend).events(account())]
    assert events[-1]["type"] == "completed" and all(c[0] == "GET" for c in script.calls)


def test_orphan_outputs_are_pruned_after_retention(tmp_path):
    import os
    import time
    ledger = Ledger(tmp_path, retention=60)
    path = ledger.cache / "old.part"
    path.write_bytes(b"private-output")
    os.utime(path, (time.time() - 61, time.time() - 61))
    ledger.prune()
    assert not path.exists()
