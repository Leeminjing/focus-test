import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from tempfile import TemporaryDirectory
from threading import Thread
import unittest

from client import RuntimeClient


class ClientTest(unittest.TestCase):
    def test_authenticated_transport_and_original_identity(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.answer()

            def do_POST(self):
                self.answer()

            def answer(self):
                if self.headers.get("Authorization") != "Bearer " + "a" * 32:
                    self.send_error(401)
                    return
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                data = json.dumps({"request_id": "request-B", "job_id": "original-job-A", "path": self.path}).encode()
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
                token_file = Path(directory) / "token"
                token_file.write_text("a" * 32, encoding="utf-8")
                client = RuntimeClient(f"http://127.0.0.1:{server.server_port}", token_file)
                self.assertEqual(client.submit({})["job_id"], "original-job-A")
                self.assertEqual(client.result("request-B")["request_id"], "request-B")
                self.assertTrue(client.events("original-job-A", 3)["path"].endswith("after_seq=3"))
                self.assertTrue(client.cancel("request-B")["path"].endswith("/cancel"))
                with self.assertRaises(ValueError):
                    RuntimeClient("http://192.0.2.1:7337", token_file)
                with self.assertRaises(ValueError):
                    client.events("original-job-A", -1)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
