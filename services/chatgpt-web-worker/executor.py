# SPDX-License-Identifier: AGPL-3.0-only
"""At-most-once submission orchestration. Cancellation never clears the fence."""
import asyncio
from contextlib import aclosing

from backend import Backend
from contract import SafeError
from conversation import Conversation


class Executor:
    def __init__(self, ledger, backend_factory=Backend):
        self.ledger, self.backend_factory = ledger, backend_factory

    def save(self, execution_id, state, stage):
        return self.ledger.update(execution_id, stage=stage, conversation_id=state.conversation_id,
                                  task_id=state.task_id, assets=state.assets)

    async def events(self, account):
        execution_id, stage = account.raw["execution_id"], "preparing"
        backend, claimed, fenced = None, False, False
        state = Conversation()
        try:
            self.ledger.claim(account)  # Admission first: rejected IDs cannot create unbounded lock files.
            with self.ledger.owner(execution_id):
                row = self.ledger.get(execution_id)  # Re-read only after exclusive ownership.
                claimed = True
                stage = row["stage"]
                fenced = stage not in {"preparing", "prepared", "failed"}
                state = Conversation(row["conversation_id"], row["task_id"], row["assets"], row["inputs"])
                if row["assets"] and row["error"] in {"content_policy_violation", "upstream_text_reply", "quota_exhausted",
                                    "auth_required", "environment_challenge", "upstream_error", "image_stream_interrupted"}:
                    state.error = row["error"]
                self.ledger.prune()
                row = self.ledger.get(execution_id)
                cached = self.ledger.replay(row)
                for image in cached:
                    yield {"type": "image", "image": image}
                if row["stage"] == "completed" and cached:
                    yield {"type": "completed", "usage_known": False}
                    return
                if row["stage"] == "failed":
                    raise SafeError(row["error"] or "upstream_error")
                if fenced and not (state.conversation_id or state.task_id or state.assets):
                    raise SafeError("submission_unknown", unknown=True)
                backend = self.backend_factory(account)
                async with asyncio.timeout(account.timeout):
                    if not fenced:
                        conduit, references = await backend.prepare()
                        state.inputs = {reference["file_id"] for reference in references}
                        self.ledger.update(execution_id, stage="prepared", inputs=sorted(state.inputs))
                        stage = "prepared"
                        yield {"type": "stage", "stage": stage}
                        self.ledger.fence(execution_id)
                        stage, fenced = "submitting", True
                        yield {"type": "stage", "stage": stage}
                        stream_error = None
                        try:
                            async with aclosing(backend.submit(conduit, references, state)) as stream:
                                async for _ in stream:
                                    next_stage = "submitted" if (state.conversation_id or state.task_id) else stage
                                    self.save(execution_id, state, next_stage)
                                    if next_stage != stage:
                                        stage = next_stage
                                        yield {"type": "stage", "stage": stage, "conversation_id": state.conversation_id,
                                               "task_id": state.task_id}
                        except Exception as error:
                            stream_error = error
                        if stream_error and not (state.assets or state.conversation_id or state.task_id):
                            if isinstance(stream_error, SafeError) and stream_error.code in {
                                    "auth_required", "environment_challenge", "quota_exhausted", "content_policy_violation"}:
                                raise stream_error
                            raise SafeError("submission_unknown", unknown=True)
                        if stream_error and state.assets:
                            state.error = state.error or "image_stream_interrupted"
                    if not state.assets and (state.conversation_id or state.task_id) and not state.error:
                        await backend.recover(state)
                        self.save(execution_id, state, "submitted")
                        stage = "submitted"
                    if not state.assets:
                        if state.error:
                            raise SafeError(state.error, 400 if state.error == "content_policy_violation" else 502)
                        if state.text_terminal:
                            raise SafeError("upstream_text_reply", 400)
                        if not (state.conversation_id or state.task_id):
                            raise SafeError("submission_unknown", unknown=True)
                        raise SafeError("image_poll_timeout")
                    self.save(execution_id, state, "generated")
                    stage = "generated"
                    yield {"type": "stage", "stage": stage, "asset_ids": state.assets}
                    stage = "downloading"
                    self.save(execution_id, state, stage)
                    yield {"type": "stage", "stage": stage}
                    images = list(cached)
                    existing = {image["asset_id"] for image in images}
                    for asset in state.assets:
                        if asset in existing:
                            continue
                        image = await backend.download(asset, state)
                        images.append(image)
                        self.ledger.store(execution_id, images)  # Persist each partial success before exposing bytes.
                        yield {"type": "image", "image": image}
                    if state.error:
                        raise SafeError(state.error)
                    stage = "completed"
                    self.ledger.update(execution_id, stage=stage, error=None, unknown=0)
                    yield {"type": "completed", "usage_known": False}
        except (asyncio.CancelledError, GeneratorExit):
            if claimed:
                unknown = fenced and not (state.conversation_id or state.task_id or state.assets)
                self.ledger.update(execution_id, stage="result_unknown" if unknown else stage,
                                   error="submission_unknown" if unknown else "image_stream_interrupted", unknown=int(unknown))
            raise
        except Exception as error:
            if not isinstance(error, SafeError):
                unknown = fenced and not (state.conversation_id or state.task_id or state.assets)
                code = "submission_unknown" if unknown else ("image_poll_timeout" if fenced else "upstream_error")
                error = SafeError(code, unknown=unknown)
            if claimed:
                terminal = error.code in {"auth_required", "quota_exhausted", "environment_challenge",
                                          "content_policy_violation", "upstream_text_reply"} and not state.assets
                saved_stage = "result_unknown" if error.unknown else ("failed" if not fenced or terminal else stage)
                self.ledger.update(execution_id, stage=saved_stage, error=error.code, unknown=int(error.unknown))
            yield error.event(stage)
        finally:
            if backend:
                await backend.transport.close()
