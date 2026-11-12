# SPDX-License-Identifier: AGPL-3.0-only
# Protocol payloads adapted from yukkcat/chatgpt2api services/openai_backend_api.py
# at 4336d0594dd4fe2962bca5b0b55bc95a24686e59. Standalone rewrite; see NOTICE.
import asyncio
import base64
import io
import time
import uuid
from datetime import datetime

from PIL import Image

from contract import MAX_OUTPUT, SafeError, image_info, safe_id
from conversation import Conversation, sse_events
from proof import Resources, proof_token, requirements_token
from transport import CurlTransport, json_response, read_bounded

BASE = "https://chatgpt.com"
VERSION = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
BUILD = "6708908"
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0")


def uid():
    return str(uuid.uuid4())


class Backend:
    def __init__(self, account, transport_factory=CurlTransport):
        self.account, self.raw = account, account.raw
        self.transport = transport_factory(account)
        self.deadline = time.monotonic() + account.timeout
        self.requirements = {}

    def remaining(self):
        value = self.deadline - time.monotonic()
        if value <= 0:
            raise TimeoutError()
        return value

    def headers(self, path):
        return {"Authorization": "Bearer " + self.raw["access_token"], "ChatGPT-Account-ID": self.raw["chatgpt_account_id"],
                "User-Agent": UA, "OAI-Device-Id": self.raw["environment"]["device_id"],
                "OAI-Session-Id": self.raw["environment"]["session_id"], "OAI-Client-Version": VERSION,
                "OAI-Client-Build-Number": BUILD, "OAI-Language": "en-US", "Origin": BASE, "Referer": BASE + "/",
                "Sec-Ch-Ua": '"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"',
                "Sec-Ch-Ua-Mobile": "?0", "Sec-Ch-Ua-Platform": '"Windows"',
                "Content-Type": "application/json", "Accept": "application/json",
                "X-OpenAI-Target-Path": path, "X-OpenAI-Target-Route": path, **self.requirements}

    async def api(self, method, path, **kwargs):
        async with self.transport.open(method, BASE + path, headers=self.headers(path), timeout=self.remaining(), **kwargs) as response:
            return await json_response(response)

    async def bootstrap(self):
        async with self.transport.open("GET", BASE + "/", headers={"User-Agent": UA, "Accept": "text/html"},
                                       timeout=self.remaining()) as response:
            html = (await read_bounded(response, 4 * 1024 * 1024)).decode("utf-8", errors="replace")
        if "cf-chl-" in html or "Just a moment..." in html:
            raise SafeError("environment_challenge", 403)
        return Resources(html)

    @staticmethod
    def supported_challenge(data):
        for name in ("arkose", "turnstile"):
            if (data.get(name) or {}).get("required"):
                raise SafeError("environment_challenge", 403)

    async def prepare_requirements(self, resources):
        path = "/backend-api/sentinel/chat-requirements"
        prepare = await self.api("POST", path + "/prepare", json={"p": requirements_token(UA, resources)})
        self.supported_challenge(prepare)
        proof = await asyncio.to_thread(proof_token, prepare.get("proofofwork") or {}, UA, resources, self.deadline)
        if not isinstance(prepare.get("prepare_token"), str) or not prepare["prepare_token"]:
            raise SafeError("environment_challenge", 403)
        final = await self.api("POST", path + "/finalize", json={"prepare_token": prepare["prepare_token"],
                               "proof_token": proof, "turnstile_token": ""})
        self.supported_challenge(final)
        if not isinstance(final.get("token"), str) or not final["token"]:
            raise SafeError("environment_challenge", 403)
        self.requirements = {"OpenAI-Sentinel-Chat-Requirements-Token": final["token"]}
        if proof:
            self.requirements["OpenAI-Sentinel-Proof-Token"] = proof
        if final.get("so_token"):
            self.requirements["OpenAI-Sentinel-SO-Token"] = final["so_token"]

    async def check(self):
        result = {"status": "unverified", "capabilities": [], "image_quota_remaining": None,
                  "quota_reset_at": None, "error_code": None}
        try:
            async with asyncio.timeout(self.account.timeout):
                await self.bootstrap()
                me = await self.api("GET", "/backend-api/me")
                if not isinstance(me.get("id"), str) or not me["id"]:
                    raise SafeError("auth_required", 401)
                init = await self.api("POST", "/backend-api/conversation/init", json={"gizmo_id": None,
                                      "requested_default_model": None, "conversation_id": None, "timezone_offset_min": -480})
                facts = {p.get("feature_name"): p for p in init.get("limits_progress", []) if isinstance(p, dict)}
                image = facts.get("image_gen", {})
                remaining = image.get("remaining")
                if type(remaining) is not int or remaining < 0:
                    return result
                result["image_quota_remaining"] = remaining
                reset = image.get("reset_after")
                if isinstance(reset, str):
                    try:
                        if datetime.fromisoformat(reset.replace("Z", "+00:00")).tzinfo:
                            result["quota_reset_at"] = reset
                    except ValueError:
                        pass
                if remaining == 0:
                    result.update(status="quota_exhausted", error_code="quota_exhausted")
                else:
                    result.update(status="ready", capabilities=["generate"])
                    if type(facts.get("image_edit", {}).get("remaining")) is int and facts["image_edit"]["remaining"] > 0:
                        result["capabilities"].append("edit")
        except SafeError as error:
            result["status"] = {"auth_required": "auth_required", "environment_challenge": "challenge",
                                "quota_exhausted": "quota_exhausted"}.get(error.code, "unverified")
            result["error_code"] = error.code
        except Exception:
            result["error_code"] = "upstream_error"
        finally:
            await self.transport.close()
        return result

    def prompt(self):
        prompt = "Use the image_generation tool to create exactly one image for the user's request.\n" + self.raw["prompt"]
        if self.raw.get("size", "auto") != "auto":
            prompt += "\nRequested image aspect ratio: " + self.raw["size"] + "."
        return prompt

    async def prepare(self):
        resources = await self.bootstrap()
        await self.prepare_requirements(resources)
        references = []
        for index, item in enumerate(self.raw.get("images", [])):
            data = base64.b64decode(item["b64_json"], validate=True)
            mime, width, height = image_info(data)
            filename = f"input-{index}." + mime.split("/")[1]
            meta = await self.api("POST", "/backend-api/files", json={"file_name": filename, "file_size": len(data),
                                  "use_case": "multimodal", "width": width, "height": height})
            file_id = safe_id(meta.get("file_id"))
            async with self.transport.open("PUT", meta.get("upload_url"), headers={"Content-Type": mime,
                                           "x-ms-blob-type": "BlockBlob", "x-ms-version": "2020-04-08"},
                                           timeout=self.remaining(), data=data, asset=True) as response:
                await read_bounded(response, 65536)
            await self.api("POST", f"/backend-api/files/{file_id}/uploaded", json={})
            references.append({"file_id": file_id, "file_name": filename, "file_size": len(data),
                               "mime_type": mime, "width": width, "height": height})
        prepared = await self.api("POST", "/backend-api/f/conversation/prepare", json={"action": "next",
                                  "fork_from_shared_post": False, "parent_message_id": uid(), "model": "gpt-5-3",
                                  "client_prepare_state": "success", "timezone_offset_min": -480, "timezone": "Asia/Shanghai",
                                  "conversation_mode": {"kind": "primary_assistant"}, "system_hints": ["picture_v2"],
                                  "partial_query": {"id": uid(), "author": {"role": "user"},
                                                    "content": {"content_type": "text", "parts": [self.prompt()]}},
                                  "supports_buffering": True, "supported_encodings": ["v1"],
                                  "client_contextual_info": {"app_name": "chatgpt.com"}})
        if not prepared.get("conduit_token"):
            raise SafeError("upstream_error")
        return prepared["conduit_token"], references

    def payload(self, references):
        parts = [{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + r["file_id"],
                  "width": r["width"], "height": r["height"], "size_bytes": r["file_size"]} for r in references]
        parts.append(self.prompt())
        metadata = {"developer_mode_connector_ids": [], "selected_github_repos": [], "selected_all_github_repos": False,
                    "system_hints": ["picture_v2"], "serialization_metadata": {"custom_symbol_offsets": []}}
        if references:
            metadata["attachments"] = [{"id": r["file_id"], "mimeType": r["mime_type"], "name": r["file_name"],
                                        "size": r["file_size"], "width": r["width"], "height": r["height"]} for r in references]
        return {"action": "next", "messages": [{"id": uid(), "author": {"role": "user"}, "create_time": time.time(),
                "content": {"content_type": "multimodal_text" if references else "text", "parts": parts}, "metadata": metadata}],
                "parent_message_id": uid(), "model": "gpt-5-3", "client_prepare_state": "sent", "timezone_offset_min": -480,
                "timezone": "Asia/Shanghai", "conversation_mode": {"kind": "primary_assistant"}, "enable_message_followups": True,
                "system_hints": ["picture_v2"], "supports_buffering": True, "supported_encodings": ["v1"],
                "client_contextual_info": {"is_dark_mode": False, "time_since_loaded": 1200, "page_height": 1072,
                 "page_width": 1724, "pixel_ratio": 1.2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"},
                "paragen_cot_summary_display_override": "allow", "force_parallel_switch": "auto"}

    async def submit(self, conduit, references, state):
        path = "/backend-api/f/conversation"
        headers = self.headers(path) | {"X-Conduit-Token": conduit, "Accept": "text/event-stream", "X-Oai-Turn-Trace-Id": uid()}
        async with self.transport.open("POST", BASE + path, headers=headers, timeout=self.remaining(), json=self.payload(references)) as response:
            async for event in sse_events(response):
                state.feed(event)
                yield state
                if state.terminal or state.error:
                    break

    async def recover(self, state):
        # One bounded, explicit recovery probe. Pending tasks return a safe error, not blind polling.
        if state.conversation_id:
            state.conversation(await self.api("GET", "/backend-api/conversation/" + safe_id(state.conversation_id)))
        if not state.assets and (state.conversation_id or state.task_id):
            state.tasks(await self.api("GET", "/backend-api/tasks"))

    async def download(self, asset, state):
        try:
            if asset.startswith("sediment://"):
                path = "/backend-api/conversation/" + safe_id(state.conversation_id) + "/attachment/" + safe_id(asset[11:]) + "/download"
            else:
                path = "/backend-api/files/" + safe_id(asset) + "/download"
            resolved = await self.api("GET", path)
            url = resolved.get("download_url") or resolved.get("url")
            async with self.transport.open("GET", url, headers={}, timeout=self.remaining(), asset=True) as response:
                data = await read_bounded(response, MAX_OUTPUT)
            mime, width, height = image_info(data)
            desired = "image/" + self.raw.get("output_format", "png")
            if mime != desired:
                with Image.open(io.BytesIO(data)) as image:
                    converted = io.BytesIO()
                    image.convert("RGB" if desired == "image/jpeg" else "RGBA").save(converted, format=desired.split("/")[1].upper())
                    data = converted.getvalue()
                if len(data) > MAX_OUTPUT:
                    raise ValueError()
                mime, width, height = image_info(data)
            return {"b64_json": base64.b64encode(data).decode(), "mime_type": mime,
                    "width": width, "height": height, "asset_id": asset}
        except SafeError as error:
            if error.code in {"auth_required", "environment_challenge", "quota_exhausted"}:
                raise
            raise SafeError("image_download_failed") from None
        except Exception:
            raise SafeError("image_download_failed") from None
