## What changed

<!-- One or two sentences. Name the file or the symbol. -->

## Why

<!-- The problem this fixes. Link the issue: Closes #NN -->

## How to verify

<!-- The exact commands or the exact clicks. -->

## Checklist

- [ ] `gofmt -l .` is empty, `go vet ./...` is clean.
- [ ] `go test -race ./...` passes with `ASYNQMON_TEST_REDIS_ADDR` set (the suites flush their databases).
- [ ] `staticcheck ./...` and `govulncheck ./...` report nothing new.
- [ ] Web UI changes: `npm run lint`, `npx tsc -b` and `npm test` pass in `ui/`.
- [ ] Web UI changes: `make assets` ran, and `ui/build` is squashed into this change's commit.
- [ ] Chart changes: `helm lint --strict charts/asynqmon` and `./scripts/helm-render-check.sh` pass.
- [ ] A new flag has a chart values key and a row in `charts/asynqmon/README.md`.
- [ ] `CHANGELOG.md` records the user-visible change.
