# SPDX-License-Identifier: AGPL-3.0-only
# Protocol adaptation of yukkcat/chatgpt2api services/protocol/conversation.py
# pinned 4336d0594dd4fe2962bca5b0b55bc95a24686e59. New bounded JSON-state parser;
# deliberately stricter than upstream: only tool content, never attachments or raw URLs.
import copy
import json
import re

from contract import SafeError, safe_id

POINTER = re.compile(r"(?:file-service|sediment)://([A-Za-z0-9_-]{1,160})")
REAL_FILE = re.compile(r"\bfile_00000000[a-f0-9]{24}\b")


def failure(payload):
    if not isinstance(payload, dict):
        return None
    error = payload.get("error")
    if isinstance(error, dict):
        code = error.get("code") or error.get("type")
    elif isinstance(error, str):
        code = error
    else:
        code = payload.get("error_code")
    aliases = {"rate_limit_exceeded": "quota_exhausted", "image_generation_limit_reached": "quota_exhausted",
               "quota_exhausted": "quota_exhausted", "invalid_token": "auth_required",
               "token_expired": "auth_required", "content_policy_violation": "content_policy_violation",
               "moderation_blocked": "content_policy_violation", "system_error": "upstream_error"}
    if code in aliases:
        return aliases[code]
    if error or payload.get("status") in {"failed", "error", "cancelled"}:
        return "upstream_error"
    if (payload.get("moderation_response") or {}).get("blocked") is True:
        return "content_policy_violation"
    return None


class Conversation:
    def __init__(self, conversation_id=None, task_id=None, assets=None, inputs=None):
        self.conversation_id, self.task_id = conversation_id, task_id
        self.assets = list(assets or [])
        self.inputs = set(inputs or [])
        self.document, self.last_path = {}, None
        self.error, self.text_terminal, self.terminal = None, False, False
        self.output_moderated = False

    def feed(self, event):
        if not isinstance(event, dict):
            return
        self.error = failure(event) or self.error
        if event.get("o") == "patch" and isinstance(event.get("v"), list):
            for op in event["v"]:
                self.feed(op)
            return
        if "p" in event or "o" in event or (set(event) == {"v"} and self.last_path):
            self.patch(event)
            self.inspect(self.document)
        else:
            value = event.get("v") if isinstance(event.get("v"), dict) else event
            if "message" in value:
                self.document = copy.deepcopy(value)
                self.last_path = None
            self.inspect(value)

    def patch(self, event):
        path = event.get("p", self.last_path)
        operation, value = event.get("o", "append"), event.get("v")
        if path is None or not isinstance(path, str):
            return
        self.last_path = path
        if path == "":
            if operation in {"add", "replace"} and isinstance(value, dict):
                self.document = copy.deepcopy(value)
            return
        if not path.startswith("/") or len(path) > 512:
            return
        keys = [p.replace("~1", "/").replace("~0", "~") for p in path[1:].split("/")]
        if len(keys) > 16:
            return
        parent = self.document
        try:
            for key in keys[:-1]:
                parent = parent[int(key)] if isinstance(parent, list) else parent[key]
            key = int(keys[-1]) if isinstance(parent, list) and keys[-1] != "-" else keys[-1]
            if operation == "append":
                old = parent[key]
                if isinstance(old, str) and isinstance(value, str):
                    parent[key] += value
                elif isinstance(old, list) and isinstance(value, list):
                    old.extend(copy.deepcopy(value))
            elif operation in {"add", "replace"}:
                if isinstance(parent, list) and key == "-":
                    parent.append(copy.deepcopy(value))
                elif isinstance(parent, list) and operation == "add":
                    parent.insert(key, copy.deepcopy(value))
                else:
                    parent[key] = copy.deepcopy(value)
            elif operation == "remove":
                del parent[key]
        except (KeyError, IndexError, ValueError, TypeError):
            return

    def inspect(self, payload):
        if not isinstance(payload, dict):
            return
        if payload.get("conversation_id"):
            candidate = safe_id(payload["conversation_id"])
            if self.conversation_id and self.conversation_id != candidate:
                raise SafeError("upstream_error")
            self.conversation_id = candidate
        if payload.get("task_id"):
            self.task_id = safe_id(payload["task_id"])
        self.error = failure(payload) or self.error
        message = payload.get("message")
        if not isinstance(message, dict):
            return
        self.error = failure(message) or self.error
        role = (message.get("author") or {}).get("role")
        metadata = message.get("metadata") or {}
        content = message.get("content") or {}
        terminal = message.get("status") in {"finished_successfully", "finished", "complete"} or message.get("end_turn") is True
        if role in {"assistant", "tool"} and terminal:
            self.terminal = True
        if role == "assistant" and terminal and message.get("recipient", "all") == "all" and content.get("content_type") == "text":
            parts = [p.strip() for p in content.get("parts", []) if isinstance(p, str) and p.strip()]
            self.text_terminal = bool(parts)
            self.output_moderated = (message.get("channel") in (None, "final", "commentary")
                                     and not metadata.get("is_visually_hidden_from_conversation")
                                     and " ".join(parts).replace("’", "'").startswith(
                                         "We're so sorry, but the image we created may violate our guardrails "
                                         "around nudity, sexuality, or erotic content."))
        if metadata.get("async_task_id"):
            self.task_id = safe_id(metadata["async_task_id"])
        if role != "tool":
            return
        text = json.dumps(content, separators=(",", ":"))
        candidates = [m.group(0) for m in POINTER.finditer(text)]
        if metadata.get("async_task_type") == "image_gen":
            candidates += REAL_FILE.findall(text)
        for pointer in candidates:
            asset = pointer if pointer.startswith("sediment://") else pointer.removeprefix("file-service://")
            bare = asset.removeprefix("sediment://")
            if bare in self.inputs or bare.startswith("file_upload"):
                continue
            if asset not in self.assets:
                if len(self.assets) >= 4:
                    raise SafeError("upstream_error")
                self.assets.append(asset)

    def conversation(self, payload):
        if payload.get("conversation_id"):
            self.inspect({"conversation_id": payload["conversation_id"]})
        for node in (payload.get("mapping") or {}).values():
            if isinstance(node, dict):
                self.inspect(node)
        self.inspect(payload)

    def tasks(self, payload):
        for task in payload.get("tasks", []):
            if not isinstance(task, dict):
                continue
            match = ((self.conversation_id and self.conversation_id in
                      {task.get("conversation_id"), task.get("original_conversation_id")}) or
                     (self.task_id and self.task_id == task.get("task_id")))
            if not match:
                continue
            if task.get("conversation_id") and not self.conversation_id:
                self.conversation_id = safe_id(task["conversation_id"])
            if task.get("task_id"):
                self.task_id = safe_id(task["task_id"])
            self.error = failure(task) or self.error
            message = task.get("image_gen_message")
            if isinstance(message, dict):
                self.inspect({"message": message})


class ChatConversation(Conversation):
    """Reconstruct visible text from full messages and delta-v1 patches."""

    def __init__(self):
        super().__init__()
        self.messages = {}
        self.model_slug = None

    def inspect(self, payload):
        self.error = failure(payload) or self.error
        message = payload.get("message")
        if not isinstance(message, dict):
            return
        self.error = failure(message) or self.error
        author, content = message.get("author") or {}, message.get("content") or {}
        metadata = message.get("metadata") or {}
        if (author.get("role") != "assistant" or message.get("recipient", "all") != "all"
                or message.get("channel") not in {None, "final"} or metadata.get("is_visually_hidden_from_conversation")
                or content.get("content_type") != "text"):
            return
        parts = content.get("parts")
        if not isinstance(parts, list) or not all(isinstance(part, str) for part in parts):
            return
        text = "".join(parts)
        message_id = message.get("id") or ""
        if message_id not in self.messages and len(self.messages) >= 128:
            raise SafeError("chat_stream_interrupted", unknown=True)
        self.messages[message_id] = text
        model = metadata.get("model_slug")
        if isinstance(model, str) and re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", model):
            self.model_slug = model
        self.text_terminal = bool(text.strip() and message.get("end_turn") is True and
                                  message.get("status") in {"finished_successfully", "finished", "complete"})


async def sse_events(response, *, chat=False):
    buffer, fields, total = b"", [], 0
    async for chunk in response.aiter_content():
        total += len(chunk)
        if total > 16 * 1024 * 1024:
            raise SafeError("image_stream_interrupted")
        buffer += chunk
        if len(buffer) > 2 * 1024 * 1024:
            raise SafeError("image_stream_interrupted")
        while b"\n" in buffer:
            line, buffer = buffer.split(b"\n", 1)
            line = line.rstrip(b"\r")
            if line.startswith(b"data:"):
                fields.append(line[5:].lstrip(b" "))
            elif not line and fields:
                data, fields = b"\n".join(fields), []
                if data == b"[DONE]":
                    if chat:
                        yield "[DONE]"
                    return
                try:
                    event = json.loads(data)
                except (ValueError, UnicodeError):
                    continue
                yield event
    if buffer or fields:
        raise SafeError("image_stream_interrupted")
