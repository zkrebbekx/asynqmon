.PHONY: api assets assets-check build docker

NODE_PATH ?= $(PWD)/ui/node_modules
# Build version stamped into the binary (--version, startup log,
# GET /api/features "version"). Override with `make build VERSION=v1.2.3`.
# The release workflow passes the git tag; -s -w drops the symbol table and
# DWARF, which the release binaries do not need.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -s -w -X main.version=$(VERSION)
assets:
	@if [ ! -d "$(NODE_PATH)"  ]; then cd ./ui && npm ci; fi
	cd ./ui && npm run build

# Rebuild the assets and fail when the result differs from the committed
# ui/build. The Go binary embeds ui/build (go:embed), so a source change
# without a rebuild ships a stale UI. CI calls this target.
assets-check: assets
	@if [ -n "$$(git status --porcelain -- ui/build)" ]; then \
		echo "ui/build is out of sync with ui/src - run 'make assets' and commit the result."; \
		git status --porcelain -- ui/build; \
		exit 1; \
	fi

# This target skips the overhead of building UI assets.
# Intended to be used during development.
api:
	go build -ldflags "$(LDFLAGS)" -o api ./cmd/asynqmon

# Build a release binary.
build: assets
	go build -trimpath -ldflags "$(LDFLAGS)" -o asynqmon ./cmd/asynqmon

# Build image and run Asynqmon server (with default settings).
docker:
	docker build -t asynqmon .
	docker run --rm \
		--name asynqmon \
		-p 8080:8080 \
		asynqmon --redis-addr=host.docker.internal:6379
