"""Experimental local runtime client for a future Focus service adapter.

This client does not authorize execution, alter Focus databases, or mark Focus
completion criteria satisfied. The existing Focus validator needs a dedicated
source/adoption implementation before these results can count as acceptance.
"""

import ipaddress
import json
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import quote, urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        return None


class RuntimeClient:
    def __init__(self, base_url, token_file):
        parsed = urlsplit(base_url)
        if parsed.scheme != "http" or parsed.username or parsed.password or parsed.path not in ("", "/") or parsed.query or parsed.fragment:
            raise ValueError("expected a literal loopback HTTP runtime address")
        if not parsed.hostname or not ipaddress.ip_address(parsed.hostname).is_loopback or not parsed.port:
            raise ValueError("expected a literal loopback IP and port")
        self.base_url = base_url.rstrip("/")
        self.token_path = Path(token_file).resolve()
        self.token = self.token_path.read_text(encoding="utf-8").strip()
        if not 26 <= len(self.token) <= 256:
            raise ValueError("invalid runtime credential")
        self.opener = build_opener(ProxyHandler({}), NoRedirect())

    def _call(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode("utf-8")
        request = Request(self.base_url + path, data=data, method=method, headers={
            "Authorization": "Bearer " + self.token,
            "Accept": "application/json", "Content-Type": "application/json",
        })
        try:
            with self.opener.open(request, timeout=10) as response:
                raw = response.read((1 << 20) + 1)
                if len(raw) > 1 << 20:
                    raise ValueError("oversized runtime response")
                return json.loads(raw)
        except HTTPError as error:
            raise RuntimeError(f"runtime HTTP {error.code}") from error

    def submit(self, descriptor):
        # Focus's service must authorize and construct the descriptor. Do not
        # grant a model the runtime credential or silently change isolation.
        return self._call("POST", "/v1/requests", descriptor)

    def capabilities(self):
        return self._call("GET", "/v1/capabilities")

    def result(self, request_id):
        return self._call("GET", "/v1/requests/" + quote(request_id, safe=""))

    def original_result(self, job_id):
        return self._call("GET", "/v1/jobs/" + quote(job_id, safe=""))

    def events(self, job_id, after_seq=0, limit=128):
        if type(after_seq) is not int or after_seq < 0:
            raise ValueError("cursor must be nonnegative")
        if type(limit) is not int or not 1 <= limit <= 256:
            raise ValueError("event limit must be 1..256")
        return self._call("GET", "/v1/jobs/" + quote(job_id, safe="") + "/events?limit=" + str(limit) + "&after_seq=" + str(after_seq))

    def event_pages(self, job_id, after_seq=0, limit=128):
        """Drain currently available pages, then return; never poll forever."""
        while True:
            page = self.events(job_id, after_seq, limit)
            if type(page.get("next_seq")) is not int or page["next_seq"] < after_seq:
                raise ValueError("invalid runtime event cursor")
            if type(page.get("has_more")) is not bool or (page["has_more"] and page["next_seq"] == after_seq):
                raise ValueError("runtime event page made no progress")
            yield page
            after_seq = page["next_seq"]
            if not page["has_more"]:
                return

    def cancel(self, request_id):
        return self._call("POST", "/v1/requests/" + quote(request_id, safe="") + "/cancel")
