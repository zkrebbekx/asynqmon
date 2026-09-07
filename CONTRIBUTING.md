# Contributing

Thank you for helping. This document lists the commands the CI runs, so you
can run them before you open a pull request.

## Prerequisites

- Go, at the version in `go.mod` (`go 1.26.0`, `toolchain go1.26.8`).
- Node 22 and npm, for the Web UI.
- A local Redis, for the integration tests.
- Helm, if you change the chart.

## Build

```bash
make assets   # build the Web UI into ui/build
make build    # build the UI, then the binary
make api      # build only the binary, for a fast backend loop
```

`make build` stamps the version into the binary. Pass your own with
`make build VERSION=v1.2.3`; the default is `devel`.

## The committed `ui/build` directory

The Go binary embeds `ui/build` with `go:embed`, so `go install` and
`go build` need those files in the repository. The directory is therefore
committed, and CI fails when it drifts from `ui/src`.

Rules:

1. Run `make assets` after every change under `ui/src`.
2. Squash the regenerated `ui/build` into the commit that changes the source.
   Do not push a separate "rebuild assets" commit. Each rebuild writes new
   content-hashed chunks, and a separate commit doubles the weight of the
   history for no benefit.
3. Run `make assets-check` before you push. The target rebuilds the assets and
   fails when the result differs from the committed files.

A pre-commit hook keeps this automatic:

```bash
cat > .git/hooks/pre-commit <<'HOOK'
#!/bin/sh
set -e
if ! git diff --cached --quiet -- ui/src; then
  make assets
  git add ui/build
fi
HOOK
chmod +x .git/hooks/pre-commit
```

## Go checks

Run all four before you push. CI runs the same commands and fails on any
finding.

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
staticcheck ./...      # go install honnef.co/go/tools/cmd/staticcheck@latest
govulncheck ./...      # go install golang.org/x/vuln/cmd/govulncheck@latest
```

### Tests that need Redis

The root package and several sub-packages are integration suites against a
real Redis. Without one they skip themselves, so a green run without Redis
proves nothing. Start a Redis and point the suites at it:

```bash
redis-server --port 6388 --daemonize yes
export ASYNQMON_TEST_REDIS_ADDR=127.0.0.1:6388
go test -race ./...
```

**The suites call `FLUSHDB` on the database they use.** Never point
`ASYNQMON_TEST_REDIS_ADDR` at a Redis that holds data you want to keep, and
never at your development queue on `127.0.0.1:6379` (the default when the
variable is unset).

The databases in use are:

| Database | Suite |
| --- | --- |
| 4 | `observed_handlers_test.go` |
| 5 | `hygiene/fence_test.go` |
| 6 | `errsig/fence_test.go` |
| 7 | `hygiene_handlers_test.go` |
| 8 | `enqueue_handlers_test.go`, `series_integration_test.go` |
| 9 | `errsig_handlers_test.go` |
| 10 | `scheduler_run_handlers_test.go`, `scheduler_snapshot_handlers_test.go` |
| 11 | `coverage_handlers_test.go` |
| 12 | `jobs_handlers_test.go` |
| 13 | `views_handlers_test.go`, `stats_handlers_test.go`, `task_detail_payload_test.go` |
| 14 | `stats/engine_test.go` |
| 15 | `internal/leasefence/leasefence_test.go` |

A new integration suite takes a database no other suite uses, and names it in
the pull request.

### Test style

Go tests use goconvey in Given/When/Then form:

```go
Convey("Given a queue with one pending task", t, func() {
    Convey("When the handler lists the queue", func() {
        Convey("Then the response holds that task", func() {
            So(len(got.Tasks), ShouldEqual, 1)
        })
    })
})
```

Every behaviour change needs a test.

## Web UI checks

```bash
cd ui
npm ci
npm run lint      # oxlint
npx tsc -b        # type check
npm test          # vitest
npm run build
```

Keep `recharts` out of the entry chunk. CI fails when
`ui/build/index.html` preloads it, because a static import of the chart
components adds the whole chart bundle to the first paint.

## Helm chart

```bash
helm lint --strict charts/asynqmon
./scripts/helm-render-check.sh
```

Add a values key for every new flag, and document the key in
`charts/asynqmon/README.md`. The render check compares the two and fails on a
mismatch.

## Commits and pull requests

- Use Conventional Commit subjects: `fix(handler): ...`, `feat(chart): ...`.
- Write the commit body in short, active sentences. Say what changed and why.
- Reference the issue as `Closes #NN`.
- One logical change per commit.
- Update `CHANGELOG.md` under "Unreleased" for a user-visible change.
