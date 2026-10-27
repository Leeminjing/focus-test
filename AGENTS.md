# Repository instructions

## Purpose and scope

Build an independent local test runtime for agents, called by Focus. Read
`README.md`, `docs/roadmap.md`, and the relevant sections of `docs/design.md`
before substantial changes. The architecture follows the original request:
Contexts -> Focus adapter -> runtime -> reuse or resource scheduling -> execution
-> progress, failures, and evidence -> Focus adapter.

Use Go's standard library by default. Do not introduce cloud test, scheduling, or
model service dependencies. Add a dependency only when a concrete implementation
need justifies it and document that decision.

## Boundaries

- Keep binary entry points small. CLI and HTTP handlers must use the same runtime
  owner for job state; do not create separate scheduling or result logic.
- `internal/protocol` owns wire types and request validation. Update
  `protocol/README.md` and examples when changing the contract.
- `internal/localnet` owns the shared local address boundary. Do not duplicate
  endpoint validation in command entry points.
- The fixture adapter simulates cases in process. Its results always retain
  `mode: "simulation"` and cannot be adopted as real test evidence.
- Requests, jobs, attempts, and result adoptions are distinct concepts. A retry
  with the same idempotency key is not cross-request reuse.
- Real test results require sealed inputs, accurate requested scope, complete
  reports, process cleanup confirmation, and trustworthy source binding. Keep
  these checks intact while adding reuse and Focus integration.
- Never relabel another Run's execution as the caller's own. Focus owns identity,
  permissions, workspace version checks, and result adoption. The runtime does
  not write Focus's database.
- Update implementation status with each milestone. Do not present planned
  snapshot, persistence, deduplication, isolation, or sandbox capabilities as
  already available. Initialization is a partial M0 foundation.

## Verification

Format changed Go files with `gofmt`. Run relevant tests while developing, then
`go vet ./...`, `go test ./...`, and `go build ./cmd/...` before delivery. Run the
race detector when the host toolchain supports it; CI covers Linux. Test
observable protocol and lifecycle behavior, especially failures and shutdown.

Use the current checkout unless isolation is needed. Do not create remote
repositories, push commits, or change a license without a user request.
