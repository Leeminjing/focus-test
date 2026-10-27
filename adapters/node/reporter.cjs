"use strict";
const fs = require("node:fs");
const path = require("node:path");

module.exports = async function* reporter(source) {
  const destination = process.env.FOCUS_TEST_EVENTS;
  let seq = 0;
  let count = 0;
  const occurrences = new Map();
  const emit = value => fs.appendFileSync(destination, JSON.stringify({version: 1, seq: ++seq, ...value}) + "\n");
  for await (const event of source) {
    const data = event.data || {};
    if (event.type === "test:stdout" || event.type === "test:stderr") {
      yield String(data.message || "");
      continue;
    }
    if (event.type !== "test:pass" && event.type !== "test:fail") continue;
    if (data.details?.type === "suite" || data.type === "suite") {
      if (event.type === "test:fail") emit({kind: "case_failed", test_id: String(data.name), message: String(data.details?.error || "suite failure")});
      continue;
    }
    const file = path.relative(process.cwd(), data.file || "").split(path.sep).join("/");
    // Node reports an empty test file itself as a passing test. It does not
    // establish that any named test was collected.
    if (event.type === "test:pass" && [file, data.file].includes(data.name)) continue;
    const base = `${file}::${data.name}::${data.line || 0}:${data.column || 0}::${data.testNumber}`;
    const occurrence = (occurrences.get(base) || 0) + 1;
    occurrences.set(base, occurrence);
    const test_id = `${base}::${occurrence}`;
    const outcome = event.type === "test:fail" ? "failed" : data.skip || data.todo ? "skipped" : "passed";
    if (outcome === "failed") emit({kind: "case_failed", test_id, message: String(data.details?.error?.stack || data.details?.error || "test failed").slice(0, 8000)});
    emit({kind: "case_finished", test_id, outcome});
    count++;
  }
  emit({kind: "report_finished", count, complete: count > 0});
};
