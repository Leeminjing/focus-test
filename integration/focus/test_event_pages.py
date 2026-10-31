import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from tempfile import TemporaryDirectory
from threading import Thread
import unittest
from urllib.parse import parse_qs, urlsplit

from client import RuntimeClient


class EventPagesTest(unittest.TestCase):
    def test_many_events_cross_response_cap_without_loss(self):
        cursors = []
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                query = parse_qs(urlsplit(self.path).query)
                after, limit = int(query["after_seq"][0]), int(query["limit"][0])
                cursors.append(after)
                events = [{"job_id": "J", "seq": seq, "kind": "case_finished", "message": "x" * 512}
                          for seq in range(after + 1, min(after + limit, 4096) + 1)]
                next_seq = events[-1]["seq"] if events else after
                data = json.dumps({"protocol_version": 1, "events": events, "next_seq": next_seq,
                                   "has_more": next_seq < 4096, "terminal": next_seq == 4096}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
            def log_message(self, *args):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with TemporaryDirectory() as directory:
                token = Path(directory) / "token"
                token.write_text("a" * 32)
                client = RuntimeClient(f"http://127.0.0.1:{server.server_port}", token)
                sequences = [event["seq"] for page in client.event_pages("J", limit=256) for event in page["events"]]
                self.assertEqual(sequences, list(range(1, 4097)))
                self.assertEqual(cursors, list(range(0, 4096, 256)))
        finally:
            server.shutdown()
            server.server_close()
            thread.join()
