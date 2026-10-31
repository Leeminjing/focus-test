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
                    request_id TEXT PRIMARY KEY, body TEXT NOT NULL,
                    next_seq INTEGER NOT NULL DEFAULT 0,
                    terminal INTEGER NOT NULL DEFAULT 0);
                CREATE TABLE IF NOT EXISTS notices (
                    identity TEXT PRIMARY KEY, request_id TEXT NOT NULL,
                    phase TEXT NOT NULL, body TEXT NOT NULL,
                    UNIQUE(request_id, phase));
                CREATE TABLE IF NOT EXISTS receipts (
                    identity TEXT NOT NULL, checkpoint_id TEXT NOT NULL,
                    response_hash TEXT NOT NULL, observed_ns INTEGER NOT NULL,
                    PRIMARY KEY(identity, checkpoint_id, response_hash));
            """)
            db.execute("BEGIN IMMEDIATE")
            if "next_seq" not in {row[1] for row in db.execute("PRAGMA table_info(bindings)")}:
                db.execute("ALTER TABLE bindings ADD COLUMN next_seq INTEGER NOT NULL DEFAULT 0")
            if "terminal" not in {row[1] for row in db.execute("PRAGMA table_info(bindings)")}:
                db.execute("ALTER TABLE bindings ADD COLUMN terminal INTEGER NOT NULL DEFAULT 0")

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
            db.execute("INSERT OR IGNORE INTO bindings(request_id,body) VALUES (?,?)", (binding["request_id"], body))

    def bindings(self, owner):
        with self.connect() as db:
            rows = db.execute("SELECT body FROM bindings ORDER BY request_id").fetchall()
        bindings = [json.loads(row[0]) for row in rows]
        return [body for body in bindings if all(body.get(key) == owner.get(key) for key in OWNER_FIELDS)]

    def cursor(self, request_id):
        return self.stream_position(request_id)[0]

    def stream_position(self, request_id):
        with self.connect() as db:
            row = db.execute("SELECT next_seq,terminal FROM bindings WHERE request_id=?", (request_id,)).fetchone()
        if row is None:
            raise ValueError("unknown trusted request binding")
        return row[0], bool(row[1])

    def observe_page(self, binding, after_seq, page):
        """Commit observations and the exact consumed cursor in one transaction.

        A concurrent/stale reader must refetch from the current cursor. No
        storage failure can advance past an uncommitted first-failure notice.
        """
        if type(after_seq) is not int or after_seq < 0 or page.get("protocol_version") != 1:
            raise ValueError("invalid event page protocol or cursor")
        events = page.get("events")
        if not isinstance(events, list) or len(events) > 256:
            raise ValueError("invalid event page size")
        seq = after_seq
        for event in events:
            seq += 1
            if event.get("job_id") != binding["job_id"] or type(event.get("seq")) is not int or event["seq"] != seq:
                raise ValueError("runtime event source mismatch")
        if (type(page.get("next_seq")) is not int or page["next_seq"] != seq
                or type(page.get("has_more")) is not bool or type(page.get("terminal")) is not bool
                or (page["has_more"] and (not events or page["terminal"]))):
            raise ValueError("event page skipped data or made no progress")
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT body,next_seq,terminal FROM bindings WHERE request_id=?", (binding["request_id"],)).fetchone()
            if row is None or row[0] != canonical(binding):
                raise ValueError("unknown or changed trusted request binding")
            if row[1] != after_seq or row[2]:
                return False
            for event in events:
                phase = "failure" if event.get("kind") in ("case_failed", "group_failed") else "finished" if event.get("kind") == "job_finished" else None
                if phase is None:
                    continue
                body = canonical({"binding": binding, "event": event, "acceptance": False})
                identity = "test-runtime:" + hashlib.sha256(body.encode()).hexdigest()
                db.execute("INSERT OR IGNORE INTO notices VALUES (?,?,?,?)", (identity, binding["request_id"], phase, body))
            db.execute("UPDATE bindings SET next_seq=?,terminal=? WHERE request_id=?", (seq, int(page["terminal"]), binding["request_id"]))
        return True

    def response_recorded(self, identity):
        with self.connect() as db:
            return db.execute("SELECT 1 FROM receipts WHERE identity=? LIMIT 1", (identity,)).fetchone() is not None

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
