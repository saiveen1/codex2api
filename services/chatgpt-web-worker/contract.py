# SPDX-License-Identifier: AGPL-3.0-only
"""Strict, secret-safe protocol validation. No caller-controlled upstream fields."""
import base64
import hashlib
import io
import json
import re
from dataclasses import dataclass
from urllib.parse import urlsplit

from PIL import Image

MAX_IMAGE = 20 * 1024 * 1024
MAX_OUTPUT = 32 * 1024 * 1024
MAX_BODY = 58 * 1024 * 1024
Image.MAX_IMAGE_PIXELS = 40_000_000
SIZES = {"auto", "1:1", "2:3", "3:2", "3:4", "4:3", "9:16", "16:9"}
WEB_IMAGE_MODELS = {
    "gpt-image-2": "gpt-5-3",
    "gpt-image-2.5": "auto",
    "gpt-image-2.5-flare": "auto",
    "gpt-image-2.5-sunburst": "auto",
    "gpt-image-2.6": "auto",
    "gpt-image-2.6-flare": "auto",
    "gpt-image-2.6-sunburst": "auto",
}
WEB_CHAT_MODELS = frozenset({"auto", "gpt-6", "gpt-6-thinking", "gpt-6-pro", "gpt-5-6", "gpt-5-6-thinking", "gpt-5-6-pro"})
WEB_CHAT_EFFORT = frozenset({"standard", "extended", "max"})
MAX_CHAT_REQUEST = 4 * 1024 * 1024  # JSON escaping can expand the bounded text and token fields.
MAX_CHAT_TEXT = 512 * 1024
MAX_CHAT_OUTPUT = 1024 * 1024
ID = re.compile(r"[A-Za-z0-9_-]{1,160}\Z")

MESSAGES = {
    "invalid_request": "Invalid worker request.",
    "unsupported_model": "Unsupported image model.",
    "unsupported_parameter": "Unsupported image parameter.",
    "chat_model_not_supported": "Unsupported ChatGPT Web text model.",
    "chat_parameter_not_supported": "Unsupported ChatGPT Web text parameter.",
    "chat_stream_interrupted": "The ChatGPT Web text stream did not complete.",
    "chat_rate_limited": "ChatGPT Web text request was rate limited.",
    "auth_required": "Upstream authentication required.",
    "environment_challenge": "Unsupported upstream environment challenge.",
    "quota_exhausted": "Upstream image quota exhausted.",
    "workspace_unavailable": "Requested upstream workspace is unavailable.",
    "identity_mismatch": "Upstream identity does not match the expected account.",
    "quota_unknown": "Upstream image quota could not be verified.",
    "worker_timeout": "Upstream account check timed out.",
    "worker_unavailable": "Upstream account check is unavailable.",
    "worker_protocol_error": "Upstream account check returned an invalid response.",
    "submission_unknown": "Submission outcome is unknown; generation will not be repeated.",
    "image_stream_interrupted": "Image stream interrupted; original execution may be resumed.",
    "image_poll_timeout": "Original image task is not ready; generation will not be repeated.",
    "image_upload_failed": "Reference image upload failed before generation was submitted.",
    "image_download_failed": "Image download failed; original assets may be downloaded again.",
    "content_policy_violation": "Upstream rejected the image request.",
    "image_output_moderated": "The upstream withheld the image under its nudity, sexuality, or erotic content policy.",
    "upstream_text_reply": "Upstream returned text instead of an image.",
    "upstream_error": "Upstream protocol request failed.",
    "execution_conflict": "Execution is active or its immutable binding does not match.",
    "capacity_exceeded": "Private worker storage capacity reached.",
}

class SafeError(Exception):
    def __init__(self, code, status=502, unknown=False, upstream_status=0, reason=""):
        self.reason = reason
        self.code, self.status, self.unknown = code, status, unknown
        self.upstream_status = upstream_status
        super().__init__(MESSAGES[code])

    def event(self, stage):
        event = {"type": "error", "code": self.code, "message": str(self), "stage": stage,
                 "http_status": self.status, "retryable": False, "result_unknown": self.unknown}
        if self.code == "environment_challenge":
            event.update(origin_status=self.upstream_status,
                         reason=self.reason or ("origin_http" if self.upstream_status else "local_verification"))
        return event


def digest(value):
    if not isinstance(value, bytes):
        value = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
    return hashlib.sha256(value).hexdigest()


def safe_id(value):
    if not isinstance(value, str) or not ID.fullmatch(value):
        raise SafeError("upstream_error")
    return value


def valid_workspace(value):
    return isinstance(value, str) and bool(ID.fullmatch(value)) and not value.lower().startswith("user-")


def image_info(data):
    try:
        with Image.open(io.BytesIO(data)) as image:
            mime = Image.MIME.get(image.format)
            width, height = image.size
            if mime not in {"image/png", "image/jpeg", "image/webp"} or width * height > Image.MAX_IMAGE_PIXELS:
                raise ValueError()
            if getattr(image, "n_frames", 1) != 1:
                raise ValueError()
            image.verify()
        with Image.open(io.BytesIO(data)) as image:
            image.load()  # Header-only JPEG verification does not prove usable pixel bytes.
        return mime, width, height
    except Exception:
        raise SafeError("invalid_request", 400) from None


@dataclass(repr=False)
class Account:
    raw: dict

    @property
    def timeout(self):
        return self.raw.get("timeout_seconds", 180)

    @classmethod
    def parse(cls, raw, execute=False, *, chat=False):
        account_fields = {"account_id", "access_token", "chatgpt_account_id", "credential_revision",
                          "environment_revision", "proxy_url", "environment", "timeout_seconds", "expected_email"}
        image_fields = {"execution_id", "operation", "model", "prompt", "images", "n", "size", "quality", "output_format"}
        chat_fields = {"model", "messages", "thinking_effort"}
        extra = chat_fields if chat else image_fields if execute else set()
        if not isinstance(raw, dict) or set(raw) - (account_fields | extra):
            raise SafeError("invalid_request", 400)
        for name in ("access_token", "credential_revision"):
            value = raw.get(name)
            if not isinstance(value, str) or not value or len(value) > 16384 or any(ord(c) < 32 for c in value):
                raise SafeError("invalid_request", 400)
        workspace = raw.get("chatgpt_account_id", "")
        if not isinstance(workspace, str) or (workspace and not valid_workspace(workspace)) or (execute and not workspace):
            raise SafeError("invalid_request", 400)
        if "expected_email" in raw:
            email = raw["expected_email"]
            if (not isinstance(email, str) or not email or len(email) > 320 or email != email.strip()
                    or "@" not in email or any(ord(c) < 33 for c in email)):
                raise SafeError("invalid_request", 400)
        for name in ("account_id", "environment_revision"):
            if type(raw.get(name)) is not int or raw[name] < 1:
                raise SafeError("invalid_request", 400)
        if raw["credential_revision"] != hashlib.sha256(raw["access_token"].encode()).hexdigest():
            raise SafeError("execution_conflict", 409)
        if type(raw.get("timeout_seconds", 180)) is not int or not 10 <= raw.get("timeout_seconds", 180) <= 600:
            raise SafeError("invalid_request", 400)
        env = raw.get("environment")
        if not isinstance(env, dict) or set(env) != {"device_id", "session_id"}:
            raise SafeError("invalid_request", 400)
        for value in env.values():
            if not isinstance(value, str) or not ID.fullmatch(value):
                raise SafeError("invalid_request", 400)
        proxy = raw.get("proxy_url", "")
        try:
            parts = urlsplit(proxy)
            if proxy and (parts.scheme not in {"http", "https", "socks5", "socks5h"} or not parts.hostname
                          or not parts.port or parts.path not in {"", "/"} or parts.query or parts.fragment
                          or any(ord(c) < 33 for c in proxy)):
                raise ValueError()
        except (ValueError, TypeError):
            raise SafeError("invalid_request", 400) from None
        if chat:
            if not workspace:
                raise SafeError("invalid_request", 400)
            model = raw.get("model")
            if not isinstance(model, str) or model not in WEB_CHAT_MODELS:
                raise SafeError("chat_model_not_supported", 400)
            effort = raw.get("thinking_effort", "")
            if not isinstance(effort, str) or (effort and effort not in WEB_CHAT_EFFORT):
                raise SafeError("chat_parameter_not_supported", 400)
            messages = raw.get("messages")
            if not isinstance(messages, list) or not 1 <= len(messages) <= 128:
                raise SafeError("invalid_request", 400)
            size = 0
            for item in messages:
                if (not isinstance(item, dict) or set(item) != {"role", "content"}
                        or not isinstance(item["role"], str) or item["role"] not in {"system", "developer", "user", "assistant"}
                        or not isinstance(item["content"], str) or not item["content"].strip()):
                    raise SafeError("invalid_request", 400)
                size += len(item["content"].encode())
            if size > MAX_CHAT_TEXT or messages[-1]["role"] != "user":
                raise SafeError("invalid_request", 400)
        if execute:
            if not isinstance(raw.get("execution_id"), str) or not ID.fullmatch(raw["execution_id"]):
                raise SafeError("invalid_request", 400)
            if not isinstance(raw.get("model"), str) or raw["model"] not in WEB_IMAGE_MODELS:
                raise SafeError("unsupported_model", 400)
            if raw.get("operation") not in {"generate", "edit"}:
                raise SafeError("invalid_request", 400)
            if (type(raw.get("n", 1)) is not int or raw.get("n", 1) != 1 or raw.get("size", "auto") not in SIZES
                    or raw.get("quality", "auto") != "auto" or raw.get("output_format", "png") not in {"png", "jpeg", "webp"}):
                raise SafeError("unsupported_parameter", 400)
            prompt = raw.get("prompt")
            if not isinstance(prompt, str) or not prompt.strip() or len(prompt.encode()) > 32_768:
                raise SafeError("invalid_request", 400)
            images = raw.get("images", [])
            if not isinstance(images, list) or len(images) > 16 or bool(images) != (raw["operation"] == "edit"):
                raise SafeError("invalid_request", 400)
            for item in images:
                try:
                    if set(item) != {"b64_json", "mime_type"} or len(item["b64_json"]) > (MAX_IMAGE * 4 // 3 + 4):
                        raise ValueError()
                    data = base64.b64decode(item["b64_json"], validate=True)
                    if not data or len(data) > MAX_IMAGE or image_info(data)[0] != item["mime_type"]:
                        raise ValueError()
                except Exception:
                    raise SafeError("invalid_request", 400) from None
        return cls(dict(raw, chatgpt_account_id=workspace))

    def binding(self):
        raw = self.raw
        payload = {k: raw.get(k, default) for k, default in
                   (("operation", None), ("model", None), ("prompt", None), ("images", []),
                    ("n", 1), ("size", "auto"), ("quality", "auto"), ("output_format", "png"))}
        fields = {"account_id": raw["account_id"], "credential_revision": raw["credential_revision"],
                  "environment_revision": raw["environment_revision"],
                  "environment_hash": digest(raw["environment"]), "proxy_hash": digest(raw.get("proxy_url", "")),
                  "chatgpt_account_hash": digest(raw["chatgpt_account_id"]), "payload_hash": digest(payload)}
        return fields | {"binding_hash": digest(fields)}
