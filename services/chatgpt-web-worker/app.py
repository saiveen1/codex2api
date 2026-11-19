# SPDX-License-Identifier: AGPL-3.0-only
"""Private HTTP contract v1; no access logs, body echoes, docs or arbitrary routes."""
import hmac
import json
import os
from contextlib import aclosing

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, StreamingResponse

from backend import Backend
from contract import Account, ID, MAX_BODY, MAX_CHAT_REQUEST, SafeError
from executor import Executor
from ledger import Ledger


def create_app(secret=None, state_dir=None, backend_factory=Backend):
    secret = secret if secret is not None else os.environ.get("CHATGPT_WEB_WORKER_SECRET", "")
    if not isinstance(secret, str) or not secret.strip():
        raise RuntimeError("CHATGPT_WEB_WORKER_SECRET must be configured")
    os.umask(0o077)
    ledger = Ledger(state_dir or os.environ.get("CHATGPT_WEB_STATE_DIR", "state"))
    executor = Executor(ledger, backend_factory)
    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
    app.state.ledger = ledger

    @app.middleware("http")
    async def authenticate(request, call_next):
        if request.url.path != "/health":
            supplied = request.headers.get("authorization", "").encode()
            expected = ("Bearer " + secret).encode()
            if not hmac.compare_digest(supplied, expected):
                diagnostics = {"stage": "admission", "upstream_status": 0} if request.url.path in {"/v1/accounts/check", "/v1/accounts/check-chat"} else {}
                return JSONResponse({"error_code": "auth_required", **diagnostics}, status_code=401)
        response = await call_next(request)
        response.headers["Cache-Control"] = "no-store"
        response.headers["X-Content-Type-Options"] = "nosniff"
        return response

    @app.exception_handler(SafeError)
    async def safe_error(request, error):
        if request.url.path in {"/v1/accounts/check", "/v1/accounts/check-chat"}:
            return JSONResponse(error.event("admission") | {"upstream_status": 0}, status_code=error.status)
        return JSONResponse(error.event("preparing"), status_code=error.status)

    async def payload(request, execute=False, *, chat=False):
        data = bytearray()
        try:
            async for chunk in request.stream():
                data.extend(chunk)
                if len(data) > (MAX_CHAT_REQUEST if chat else MAX_BODY):
                    raise SafeError("invalid_request", 413)
            return Account.parse(json.loads(data), execute, chat=chat)
        except SafeError:
            raise
        except Exception:
            raise SafeError("invalid_request", 400) from None

    @app.get("/health")
    async def health():
        return {"status": "ok", "protocol_version": "1"}

    @app.post("/v1/accounts/check")
    async def check(request: Request):
        account = await payload(request)
        return await backend_factory(account).check()

    @app.post("/v1/accounts/check-chat")
    async def check_chat(request: Request):
        account = await payload(request)
        return await backend_factory(account).check(chat=True)

    @app.post("/v1/chat/execute")
    async def execute_chat(request: Request):
        account = await payload(request, chat=True)

        async def stream():
            async with aclosing(backend_factory(account).chat_events()) as events:
                async for event in events:
                    if await request.is_disconnected():
                        return
                    yield json.dumps(event, separators=(",", ":")) + "\n"
        return StreamingResponse(stream(), media_type="application/x-ndjson", headers={"X-Accel-Buffering": "no"})

    @app.post("/v1/images/execute")
    async def execute(request: Request):
        account = await payload(request, True)

        async def stream():
            async with aclosing(executor.events(account)) as events:
                async for event in events:
                    if await request.is_disconnected():
                        return
                    yield json.dumps(event, separators=(",", ":")) + "\n"
        return StreamingResponse(stream(), media_type="application/x-ndjson", headers={"X-Accel-Buffering": "no"})

    @app.get("/v1/executions/{execution_id}")
    async def execution(execution_id: str):
        if not ID.fullmatch(execution_id):
            raise SafeError("invalid_request", 400)
        result = ledger.public(execution_id)
        if result is None:
            return JSONResponse({"error_code": "not_found"}, status_code=404)
        return result

    return app
