import tempfile
from pathlib import Path
import unittest

from feedback_store import FeedbackStore, OWNER_FIELDS


class FeedbackStoreTest(unittest.TestCase):
    def test_immutable_binding_replay_and_owner_isolation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "feedback.sqlite"
            store = FeedbackStore(path)
            binding = {key: "A" for key in OWNER_FIELDS}
            binding.update(request_id="request-A", job_id="job-shared", snapshot_id="snapshot", execution_key="key", call_id="call-A", workspace_revision="revision")
            store.register(binding)
            with self.assertRaises(ValueError):
                store.register({**binding, "run_id": "B"})
            events = [{"job_id": "job-shared", "seq": 1, "kind": "group_failed"}, {"job_id": "job-shared", "seq": 2, "kind": "case_failed"}]
            store.observe(binding, events)
            store = FeedbackStore(path)
            store.observe(binding, events)
            self.assertEqual(len(store.notices(binding)), 1)
            self.assertEqual(store.bindings({**binding, "run_id": "B"}), [])
            self.assertEqual(store.bindings(binding), [binding])
            self.assertEqual(store.receipts(), [])
            with self.assertRaises(ValueError):
                store.observe(binding, [{**events[0], "job_id": "different"}])
            identity = store.notices(binding)[0][0]
            store.record_response(identity, "exact-checkpoint", "response")
            store.record_response(identity, "exact-checkpoint", "response")
            self.assertEqual(len(store.receipts()), 1)
            self.assertFalse(store.notices(binding)[0][1]["acceptance"])
