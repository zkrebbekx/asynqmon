.PHONY: api assets build docker

NODE_PATH ?= $(PWD)/ui/node_modules
# Build version stamped into the binary (--version, startup log,
# GET /api/features "version"). Override with `make build VERSION=v1.2.3`.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -X main.version=$(VERSION)
assets:
	@if [ ! -d "$(NODE_PATH)"  ]; then cd ./ui && npm ci; fi
	cd ./ui && npm run build

# This target skips the overhead of building UI assets.
# Intended to be used during development.
api:
	go build -ldflags "$(LDFLAGS)" -o api ./cmd/asynqmon

# Build a release binary.
build: assets
	go build -ldflags "$(LDFLAGS)" -o asynqmon ./cmd/asynqmon

# Build image and run Asynqmon server (with default settings).
docker:
	docker build -t asynqmon .
	docker run --rm \
		--name asynqmon \
		-p 8080:8080 \
		asynqmon --redis-addr=host.docker.internal:6379
