# SPDX-License-Identifier: AGPL-3.0-only
"""Durable submission fence. Fences never expire; only private output bytes do."""
import json
import os
import sqlite3
import time
from contextlib import contextmanager
from pathlib import Path

from contract import SafeError, digest


class Ledger:
    def __init__(self, root, retention=86400, max_cache=2 * 1024**3, max_records=100000):
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.cache = self.root / "outputs"
        self.locks = self.root / "locks"
        self.cache.mkdir(exist_ok=True, mode=0o700)
        self.locks.mkdir(exist_ok=True, mode=0o700)
        self.retention, self.max_cache, self.max_records = retention, max_cache, max_records
        self.db = self.root / "ledger.sqlite3"
        with self.connect() as db:
            db.execute("PRAGMA journal_mode=WAL")
            db.execute("CREATE TABLE IF NOT EXISTS executions (id TEXT PRIMARY KEY, binding TEXT NOT NULL, "
                       "stage TEXT NOT NULL, conversation_id TEXT, task_id TEXT, assets TEXT NOT NULL DEFAULT '[]', "
                       "inputs TEXT NOT NULL DEFAULT '[]', error TEXT, unknown INTEGER NOT NULL DEFAULT 0, "
                       "cache TEXT NOT NULL DEFAULT '[]', bytes INTEGER NOT NULL DEFAULT 0, updated REAL NOT NULL)")
        if os.name != "nt":
            os.chmod(self.db, 0o600)

    @contextmanager
    def connect(self):
        db = sqlite3.connect(self.db, timeout=5)
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA synchronous=FULL")
        try:
            with db:
                yield db
        finally:
            db.close()

    @contextmanager
    def owner(self, execution_id):
        path = self.locks / (digest(execution_id) + ".lock")
        handle = open(path, "a+b")
        try:
            if os.name == "nt":
                import msvcrt
                handle.seek(0)
                if not handle.read(1):
                    handle.write(b"0")
                    handle.flush()
                handle.seek(0)
                msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            handle.close()
            raise SafeError("execution_conflict", 409) from None
        try:
            yield
        finally:
            handle.close()  # OS releases ownership on close, cancellation and process death.

    def get(self, execution_id):
        with self.connect() as db:
            row = db.execute("SELECT * FROM executions WHERE id=?", (execution_id,)).fetchone()
        if not row:
            return None
        row = dict(row)
        for key in ("binding", "assets", "inputs", "cache"):
            row[key] = json.loads(row[key])
        return row

    def claim(self, account):
        execution_id, binding = account.raw["execution_id"], account.binding()
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT binding FROM executions WHERE id=?", (execution_id,)).fetchone()
            if row:
                if json.loads(row["binding"]) != binding:
                    raise SafeError("execution_conflict", 409)
            else:
                if db.execute("SELECT count(*) FROM executions").fetchone()[0] >= self.max_records:
                    raise SafeError("capacity_exceeded", 503)
                db.execute("INSERT INTO executions(id,binding,stage,updated) VALUES(?,?,'preparing',?)",
                           (execution_id, json.dumps(binding), time.time()))
        return self.get(execution_id)

    def update(self, execution_id, **fields):
        allowed = {"stage", "conversation_id", "task_id", "assets", "inputs", "error", "unknown", "cache", "bytes"}
        assert fields and set(fields) <= allowed
        fields["updated"] = time.time()
        values = [json.dumps(v) if k in {"assets", "inputs", "cache"} else v for k, v in fields.items()]
        with self.connect() as db:
            db.execute("UPDATE executions SET " + ",".join(k + "=?" for k in fields) + " WHERE id=?", values + [execution_id])
        return self.get(execution_id)

    def fence(self, execution_id):
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            result = db.execute("UPDATE executions SET stage='submitting',updated=? WHERE id=? AND stage='prepared'",
                                (time.time(), execution_id))
            if result.rowcount != 1:
                raise SafeError("execution_conflict", 409)
        # FULL synchronous commit completes before the caller is permitted to POST generation.

    def prune(self):
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            rows = db.execute("SELECT id,cache FROM executions WHERE bytes>0 AND updated<?",
                              (time.time() - self.retention,)).fetchall()
            for row in rows:
                for name in json.loads(row["cache"]):
                    (self.cache / name).unlink(missing_ok=True)
                db.execute("UPDATE executions SET cache='[]',bytes=0 WHERE id=?", (row["id"],))
            referenced = {name for row in db.execute("SELECT cache FROM executions WHERE bytes>0")
                          for name in json.loads(row[0])}
            for path in self.cache.iterdir():
                if path.name not in referenced and path.is_file() and path.stat().st_mtime < time.time() - self.retention:
                    path.unlink(missing_ok=True)

    def store(self, execution_id, images):
        self.prune()
        encoded = [json.dumps(image, separators=(",", ":")).encode() for image in images]
        total = sum(map(len, encoded))
        names = [digest(execution_id) + f"-{i}.json" for i in range(len(images))]
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            # Count actual files too: crash-orphan bytes must not escape disk admission.
            used = sum(path.stat().st_size for path in self.cache.iterdir() if path.is_file())
            previous = set(json.loads(db.execute("SELECT cache FROM executions WHERE id=?", (execution_id,)).fetchone()[0]))
            additions = [(name, data) for name, data in zip(names, encoded) if name not in previous]
            if used + sum(len(data) for _, data in additions) > self.max_cache:
                raise SafeError("capacity_exceeded", 503)
            for name, data in additions:
                path, temporary = self.cache / name, self.cache / (name + ".part")
                fd = os.open(temporary, os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o600)
                with os.fdopen(fd, "wb") as output:
                    output.write(data)
                    output.flush()
                    os.fsync(output.fileno())
                os.replace(temporary, path)
            db.execute("UPDATE executions SET cache=?,bytes=?,updated=? WHERE id=?",
                       (json.dumps(names), total, time.time(), execution_id))

    def replay(self, row):
        try:
            return [json.loads((self.cache / name).read_bytes()) for name in row["cache"]]
        except (OSError, ValueError):
            self.update(row["id"], cache=[], bytes=0)
            return []  # Missing cache never authorizes another generation; only original asset downloads.

    def public(self, execution_id):
        row = self.get(execution_id)
        if not row:
            return None
        if row["stage"] == "submitting" and not (row["conversation_id"] or row["task_id"] or row["assets"]):
            try:
                with self.owner(execution_id):
                    row = self.get(execution_id)
                    if row["stage"] == "submitting" and not (row["conversation_id"] or row["task_id"] or row["assets"]):
                        row["stage"], row["unknown"], row["error"] = "result_unknown", 1, "submission_unknown"
            except SafeError:
                pass  # Active owner may still receive recoverable IDs; GET does not mutate its state.
        return {"execution_id": row["id"], "stage": row["stage"], **row["binding"],
                "conversation_id": row["conversation_id"], "task_id": row["task_id"], "asset_ids": row["assets"],
                "error_code": row["error"], "result_unknown": bool(row["unknown"]),
                "result_available": bool(row["cache"]) and row["updated"] >= time.time() - self.retention}
