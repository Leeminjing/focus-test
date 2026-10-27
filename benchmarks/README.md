# Coordination benchmark

Run `go run ./cmd/focus-test-bench --output benchmarks/latest.json` from the
repository root. The command uses a private temporary directory and no external
service. It compares 1, 2, 4, and 8 simulated Contexts at the same admission
budget (2 CPU units, 512 MiB), with reuse disabled and with inflight reuse.

The output contains actual observed elapsed times and actual job counts. Inputs
are sealed before coordination timing; cold and warm content-store sealing
costs are reported separately. These are in-process simulated tests with one
measurement per cell. There is no model consumption measurement, application
test acceleration claim, persistent result cache, or measured CPU/memory usage.
Use this experiment to validate coordination mechanics, not to estimate ROI.

The measured engine uses in-memory events to isolate scheduling and merging;
durable journal sync latency is excluded. Cold/warm sealing describes an empty
versus populated content store, not a cleared operating-system disk cache.

Pass `--revision <git-commit>` to retain the tested source revision. The report
also records OS, architecture and Go version. Without the flag, revision is
explicitly `unrecorded`.
