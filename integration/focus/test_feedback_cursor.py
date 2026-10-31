from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import sqlite3
from tempfile import TemporaryDirectory
import unittest

from feedback_store import FeedbackStore, OWNER_FIELDS
from test_feedback_store import page


class CursorTest(unittest.TestCase):
    def binding(self, request):
        return {**{key: request for key in OWNER_FIELDS}, "request_id": request, "job_id": "shared",
                "snapshot_id": "snapshot", "execution_key": "key", "call_id": "call", "workspace_revision": "v1"}

    def test_atomic_rollback_restart_replay_and_independent_subscribers(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / "feedback.sqlite"
            store = FeedbackStore(path)
            a, b = self.binding("A"), self.binding("B")
            store.register(a)
            store.register(b)
            events = [{"job_id": "shared", "seq": 1, "kind": "case_failed"}]
            with store.connect() as db:
                db.execute("CREATE TRIGGER fail_cursor BEFORE UPDATE OF next_seq ON bindings BEGIN SELECT RAISE(ABORT,'injected commit failure'); END")
            with self.assertRaises(sqlite3.IntegrityError):
                store.observe_page(a, 0, page(events))
            store = FeedbackStore(path)
            self.assertEqual(store.cursor("A"), 0)
            self.assertEqual(store.notices(a), [])
            with store.connect() as db:
                db.execute("DROP TRIGGER fail_cursor")
            with ThreadPoolExecutor(max_workers=8) as pool:
                committed = list(pool.map(lambda _: store.observe_page(a, 0, page(events)), range(16)))
            self.assertEqual(sum(committed), 1)
            self.assertEqual(len(store.notices(a)), 1)
            self.assertEqual(store.cursor("B"), 0)
            self.assertTrue(store.observe_page(b, 0, page(events)))
            self.assertNotEqual(store.notices(a)[0][0], store.notices(b)[0][0])
            self.assertEqual(FeedbackStore(path).cursor("A"), 1)
            with self.assertRaises(ValueError):
                store.observe_page(a, 1, page([{**events[0], "seq": 3}]))
            self.assertEqual(store.cursor("A"), 1)
            finished = {"job_id": "shared", "seq": 2, "kind": "job_finished"}
            self.assertTrue(store.observe_page(a, 1, page([finished])))
            self.assertEqual(FeedbackStore(path).stream_position("A"), (2, True))
            self.assertEqual(store.stream_position("B"), (1, False))

    def test_existing_schema_migrates_without_losing_notices(self):
        with TemporaryDirectory() as directory:
            path = Path(directory) / "old.sqlite"
            a = self.binding("A")
            with sqlite3.connect(path) as db:
                db.execute("CREATE TABLE bindings(request_id TEXT PRIMARY KEY,body TEXT NOT NULL)")
                db.execute("INSERT INTO bindings VALUES (?,?)", ("A", json.dumps(a,sort_keys=True,separators=(",",":"))))
            db.close()
            store = FeedbackStore(path)
            self.assertEqual(store.cursor("A"), 0)
            store.register(a)
