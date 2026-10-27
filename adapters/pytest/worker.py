"""Run pytest with a dedicated JSONL event channel and complete phase accounting."""

import json
import os
import sys

import pytest


class Reporter:
    def __init__(self, path):
        self.stream = open(path, "a", encoding="utf-8", buffering=1)
        self.seq = 0
        self.collected = None
        self.collection_failed = False
        self.phases = {}
        self.finished = set()

    def emit(self, **event):
        self.seq += 1
        self.stream.write(json.dumps({"version": 1, "seq": self.seq, **event}) + "\n")
        self.stream.flush()

    def pytest_collectreport(self, report):
        if report.failed:
            self.collection_failed = True
            self.emit(kind="case_failed", test_id=report.nodeid, message=str(report.longrepr)[-8000:])

    def pytest_collection_finish(self, session):
        self.collected = {item.nodeid for item in session.items}

    def pytest_runtest_logreport(self, report):
        phases = self.phases.setdefault(report.nodeid, [])
        phases.append(report.outcome)
        if report.failed:
            self.emit(kind="case_failed", test_id=report.nodeid, message=str(report.longrepr)[-8000:])
        if report.when == "teardown":
            outcome = "failed" if "failed" in phases else "skipped" if "skipped" in phases else "passed"
            self.finished.add(report.nodeid)
            self.emit(kind="case_finished", test_id=report.nodeid, outcome=outcome)

    def pytest_sessionfinish(self, session, exitstatus):
        complete = bool(self.collected) and self.collected == self.finished
        complete = complete and not self.collection_failed and int(exitstatus) in (0, 1)
        self.emit(kind="report_finished", count=len(self.finished), complete=complete)


if __name__ == "__main__":
    sys.path.insert(0, os.getcwd())
    reporter = Reporter(os.environ["FOCUS_TEST_EVENTS"])
    try:
        raise SystemExit(pytest.main(sys.argv[1:], plugins=[reporter]))
    finally:
        reporter.stream.close()
