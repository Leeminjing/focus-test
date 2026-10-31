"use strict";
const fs = require("node:fs");
const path = require("node:path");

module.exports = async function* reporter(source) {
  const destination = process.env.FOCUS_TEST_EVENTS;
  let seq = 0;
  let count = 0;
  const occurrences = new Map();
  const completedIdentities = new Map();
  const announcedFailures = new Set();
  const failedFiles = new Set();
  const relativeFile = data => path.relative(process.cwd(), data.entryFile || data.file || "").split(path.sep).join("/");
  const emit = value => fs.appendFileSync(destination, JSON.stringify({version: 1, seq: ++seq, ...value}) + "\n");
  const identity = (data, completed, failed) => {
    const file = relativeFile(data);
    if (Number.isInteger(data.testId)) return `${file}::${data.name}::node:${data.testId}`;
    // Older Node versions renumber testNumber when forwarding child events.
    // Pair by declaration site and outcome instead; repeated declarations at
    // one site receive distinct identities, even when they finish out of order.
    const base = `${file}::${data.name}::${data.line || 0}:${data.column || 0}:${data.nesting || 0}`;
    const key = `${base}:${failed}:${Boolean(data.skip)}:${Boolean(data.todo)}`;
    const pending = completedIdentities.get(key) || [];
    if (!completed && pending.length) return pending.shift();
    const occurrence = (occurrences.get(base) || 0) + 1;
    occurrences.set(base, occurrence);
    const id = `${base}::${occurrence}`;
    if (completed) {
      pending.push(id);
      completedIdentities.set(key, pending);
    }
    return id;
  };
  const fail = (data, test_id) => {
    if (announcedFailures.has(test_id)) return;
    announcedFailures.add(test_id);
    failedFiles.add(relativeFile(data));
    emit({kind: "case_failed", test_id, message: String(data.details?.error?.cause?.stack || data.details?.error?.stack || data.details?.error || "test failed").slice(0, 8000)});
  };
  for await (const event of source) {
    const data = event.data || {};
    if (event.type === "test:stdout" || event.type === "test:stderr") {
      yield String(data.message || "");
      continue;
    }
    // Completion is execution ordered. pass/fail below are declaration ordered
    // and may remain buffered behind an unrelated slow file.
    if (event.type === "test:complete") {
      const file = relativeFile(data);
      // With process isolation, Node can publish a file's completion before
      // forwarding its named test events. This is a group diagnostic, never an
      // additional failed case. Keep the named failures in the final report.
      const fileCompletion = [file, String(data.file).split(path.sep).join("/")].includes(String(data.name).split(path.sep).join("/"));
      if (fileCompletion) {
        if (data.details?.passed === false && !failedFiles.has(file)) {
          failedFiles.add(file);
          emit({kind: "group_failed", test_id: file, message: "Test file failed; named case details follow in the final report."});
        }
        continue;
      }
      const test_id = identity(data, true, data.details?.passed === false);
      if (data.details?.passed === false && !data.skip && !data.todo) fail(data, test_id);
      continue;
    }
    if (event.type !== "test:pass" && event.type !== "test:fail") continue;
    const test_id = identity(data, false, event.type === "test:fail");
    if (data.details?.type === "suite" || data.type === "suite") {
      if (event.type === "test:fail") fail(data, test_id);
      continue;
    }
    const file = path.relative(process.cwd(), data.file || "").split(path.sep).join("/");
    // Node reports an empty test file itself as a passing test. It does not
    // establish that any named test was collected.
    if (event.type === "test:pass" && [file, data.file].includes(data.name)) continue;
    const outcome = event.type === "test:fail" ? "failed" : data.skip || data.todo ? "skipped" : "passed";
    if (outcome === "failed") fail(data, test_id);
    emit({kind: "case_finished", test_id, outcome});
    count++;
  }
  emit({kind: "report_finished", count, complete: count > 0});
};
