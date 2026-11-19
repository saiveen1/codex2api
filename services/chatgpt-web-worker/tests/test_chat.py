# SPDX-License-Identifier: AGPL-3.0-only
import json
from contextlib import asynccontextmanager

import httpx
import pytest

from app import create_app
from test_worker import DISCOVERY, Script, check_account, deny_network, sse  # noqa: F401


class ChatScript(Script):
    @asynccontextmanager
    async def open(self, method, url, **kwargs):
        async with super().open(method, url, **kwargs) as response:
            response.headers = {"content-type": "text/event-stream"} if url.endswith("/f/conversation") else {}
            yield response


def chat_request(model="gpt-6-thinking"):
    return check_account(expected_email="owner@example.com").raw | {
        "model": model, "thinking_effort": "extended",
        "messages": [{"role": "system", "content": "test instruction"},
                     {"role": "user", "content": "previous question"},
                     {"role": "assistant", "content": "previous answer"},
                     {"role": "user", "content": "current question"}],
    }


def assistant(text, **changes):
    return {"message": {"id": "msg-1", "author": {"role": "assistant"}, "recipient": "all",
                        "content": {"content_type": "text", "parts": [text]},
                        "metadata": {"model_slug": "gpt-6-thinking"}, **changes}}


@pytest.mark.asyncio
async def test_web_chat_preserves_model_roles_and_visible_deltas(tmp_path):
    script = ChatScript(sse(assistant("private reasoning", id="hidden", channel="analysis"),
                            assistant("你好"), {"p": "/message/content/parts/0", "o": "append", "v": "，世界"},
                            assistant("你好，世界", status="finished_successfully", end_turn=True)))
    app = create_app("test-secret", tmp_path, script.backend)
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://worker") as client:
        response = await client.post("/v1/chat/execute", json=chat_request(),
                                     headers={"Authorization": "Bearer test-secret"})
    assert response.status_code == 200
    assert response.headers["content-type"].startswith("application/x-ndjson")
    assert [json.loads(line) for line in response.iter_lines()] == [
        {"type": "delta", "text": "你好"}, {"type": "delta", "text": "，世界"},
        {"type": "completed", "model_slug": "gpt-6-thinking", "usage_known": False},
    ]
    assert len(script.submits()) == 1 and script.closed
    prepare = next(call[2] for call in script.calls if call[1].endswith("/f/conversation/prepare"))
    submit = script.submits()[0][2]
    assert prepare["json"]["parent_message_id"] == submit["json"]["parent_message_id"]
    assert prepare["json"]["partial_query"] == submit["json"]["messages"][-1]
    assert submit["headers"]["ChatGPT-Account-ID"] == "mock-account"
    for options in (prepare, submit):
        assert options["json"]["model"] == "gpt-6-thinking"
        assert options["json"]["thinking_effort"] == "extended"
        assert options["json"]["system_hints"] == []
    assert [msg["author"]["role"] for msg in submit["json"]["messages"]] == ["system", "user", "assistant", "user"]


@pytest.mark.asyncio
async def test_web_chat_disconnect_is_not_completion_or_replay(tmp_path):
    script = ChatScript(sse(assistant("partial")).removesuffix(b"data: [DONE]\n\n"))
    app = create_app("test-secret", tmp_path, script.backend)
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://worker") as client:
        response = await client.post("/v1/chat/execute", json=chat_request(),
                                     headers={"Authorization": "Bearer test-secret"})
    events = [json.loads(line) for line in response.iter_lines()]
    assert [event["type"] for event in events] == ["delta", "error"]
    assert events[-1]["code"] == "chat_stream_interrupted" and events[-1]["result_unknown"]
    assert len(script.submits()) == 1 and script.closed


@pytest.mark.asyncio
async def test_web_chat_check_never_reads_image_quota(tmp_path):
    script = ChatScript()
    script.quota = None
    app = create_app("test-secret", tmp_path, script.backend)
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://worker") as client:
        response = await client.post("/v1/accounts/check-chat", json=check_account().raw,
                                     headers={"Authorization": "Bearer test-secret"})
    assert response.status_code == 200
    result = response.json()
    assert result["status"] == "chat_ready" and result["capabilities"] == ["chat"]
    assert result["image_quota_remaining"] is None and result["workspace_id"] == "mock-account"
    assert [url for _, url, _ in script.calls] == ["https://chatgpt.com/", "https://chatgpt.com/backend-api/me", DISCOVERY]
    assert script.closed


@pytest.mark.asyncio
async def test_web_chat_rejects_unknown_model_and_checks_worker_secret(tmp_path):
    script = ChatScript()
    app = create_app("test-secret", tmp_path, script.backend)
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://worker") as client:
        unauthorized = await client.post("/v1/chat/execute", json=chat_request())
        invalid = await client.post("/v1/chat/execute", json=chat_request("gpt-6.2-astra"),
                                    headers={"Authorization": "Bearer test-secret"})
    assert unauthorized.status_code == 401
    assert invalid.status_code == 400 and invalid.json()["code"] == "chat_model_not_supported"
    assert not script.calls


@pytest.mark.asyncio
async def test_web_chat_pro_keeps_slug_and_uses_captured_default_effort():
    from contract import Account

    script = ChatScript(sse(assistant("complete", status="finished_successfully", end_turn=True)))
    payload = chat_request("gpt-6-pro")
    payload.pop("thinking_effort")
    events = [event async for event in script.backend(Account.parse(payload, chat=True)).chat_events()]
    assert events[-1]["type"] == "completed"
    submit = script.submits()[0][2]["json"]
    assert submit["model"] == "gpt-6-pro" and submit["thinking_effort"] == "standard"
