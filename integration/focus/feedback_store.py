"""App-private advisory delivery journal; never a Focus acceptance database.

The Focus service registers trusted request bindings. No model-facing operation
can register a binding, select another Run, or obtain a runtime credential.
"""

import hashlib
from contextlib import contextmanager
import json
from pathlib import Path
import sqlite3
import time


OWNER_FIELDS = ("thread_id", "checkpoint_ns", "run_id", "agent_id", "task_id", "workspace_id")


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


class FeedbackStore:
    def __init__(self, path):
        self.path = Path(path).resolve()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with self.connect() as db:
            db.executescript("""
                CREATE TABLE IF NOT EXISTS bindings (
                    request_id TEXT PRIMARY KEY, body TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS notices (
                    identity TEXT PRIMARY KEY, request_id TEXT NOT NULL,
                    phase TEXT NOT NULL, body TEXT NOT NULL,
                    UNIQUE(request_id, phase));
                CREATE TABLE IF NOT EXISTS receipts (
                    identity TEXT NOT NULL, checkpoint_id TEXT NOT NULL,
                    response_hash TEXT NOT NULL, observed_ns INTEGER NOT NULL,
                    PRIMARY KEY(identity, checkpoint_id, response_hash));
            """)

    @contextmanager
    def connect(self):
        db = sqlite3.connect(self.path, timeout=10)
        try:
            db.execute("PRAGMA synchronous=FULL")
            with db:
                yield db
        finally:
            db.close()

    def register(self, binding):
        required = (*OWNER_FIELDS, "request_id", "job_id", "snapshot_id", "execution_key", "call_id", "workspace_revision")
        if any(not isinstance(binding.get(key), str) or not binding[key] for key in required if key != "checkpoint_ns"):
            raise ValueError("incomplete trusted Focus request binding")
        if not isinstance(binding.get("checkpoint_ns"), str):
            raise ValueError("checkpoint namespace is required")
        body = canonical(binding)
        with self.connect() as db:
            old = db.execute("SELECT body FROM bindings WHERE request_id=?", (binding["request_id"],)).fetchone()
            if old and old[0] != body:
                raise ValueError("request binding is immutable")
            db.execute("INSERT OR IGNORE INTO bindings VALUES (?,?)", (binding["request_id"], body))

    def bindings(self, owner):
        with self.connect() as db:
            rows = db.execute("SELECT body FROM bindings ORDER BY request_id").fetchall()
        bindings = [json.loads(row[0]) for row in rows]
        return [body for body in bindings if all(body.get(key) == owner.get(key) for key in OWNER_FIELDS)]

    def observe(self, binding, events):
        for event in events:
            if event.get("job_id") != binding["job_id"] or not isinstance(event.get("seq"), int) or event["seq"] < 1:
                raise ValueError("runtime event source mismatch")
            phase = "failure" if event.get("kind") in ("case_failed", "group_failed") else "finished" if event.get("kind") == "job_finished" else None
            if phase is None:
                continue
            observation = {"binding": binding, "event": event, "acceptance": False}
            body = canonical(observation)
            identity = "test-runtime:" + hashlib.sha256(body.encode()).hexdigest()
            # First failure is stable across cursor replays, restart and later
            # named case details; final settlement is a separate observation.
            with self.connect() as db:
                db.execute("INSERT OR IGNORE INTO notices VALUES (?,?,?,?)", (identity, binding["request_id"], phase, body))

    def notices(self, binding):
        with self.connect() as db:
            rows = db.execute("SELECT identity, body FROM notices WHERE request_id=? ORDER BY rowid", (binding["request_id"],)).fetchall()
        return [(identity, json.loads(body)) for identity, body in rows]

    def record_response(self, identity, checkpoint_id, response_hash):
        if not checkpoint_id or not response_hash:
            raise ValueError("exact checkpoint and observed model response required")
        with self.connect() as db:
            if db.execute("SELECT 1 FROM notices WHERE identity=?", (identity,)).fetchone() is None:
                raise ValueError("unknown observation")
            db.execute("INSERT OR IGNORE INTO receipts VALUES (?,?,?,?)", (identity, checkpoint_id, response_hash, time.time_ns()))

    def receipts(self):
        with self.connect() as db:
            return db.execute("SELECT identity,checkpoint_id,response_hash,observed_ns FROM receipts ORDER BY observed_ns").fetchall()
