#
# First stage:
# Building a frontend.
#

# Multi-arch build (hibiken/asynqmon#292): both build stages are pinned to the
# BUILD platform so npm/go run natively (no QEMU emulation); Go cross-compiles
# to the TARGET platform via TARGETOS/TARGETARCH below. The frontend bundle is
# architecture-independent.
# Base images are digest-pinned so a rebuild of the same commit gives the
# same toolchain. Dependabot (docker ecosystem) bumps the digests. Resolve a
# new digest with: docker buildx imagetools inspect <image:tag>
FROM --platform=$BUILDPLATFORM node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32 AS frontend

# Move to a working directory (/static).
WORKDIR /static

# Copy only ./ui folder to the working directory.
COPY ui .

# The committed ui/build (embedded by the Go binary for non-Docker builds) is
# excluded via .dockerignore; a directory COPY *merges*, so letting it in
# would leave stale content-hashed chunks alongside the fresh bundle below.
# Install dependencies from the lockfile and build the Vite bundle (-> ./build).
RUN npm ci && npm run build

#
# Second stage:
# Building a backend.
#

FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS backend

# CA bundle for the final scratch image: the binary dials TLS Redis
# (--redis-tls / rediss:// URLs) and HTTPS Prometheus (--prometheus-addr),
# and scratch ships no root CAs — without this every documented TLS feature
# fails x509 verification and pushes users toward --redis-insecure-tls.
RUN apk add --no-cache ca-certificates

# Move to a working directory (/build).
WORKDIR /build

# Copy and download dependencies.
COPY go.mod go.sum ./
RUN go mod download

# Copy a source code to the container.
COPY . .

# Copy frontend static files from /static to the root folder of the backend container.
COPY --from=frontend ["/static/build", "ui/build"]

# Cross-compile for the platform requested via `docker buildx --platform`
# (hibiken/asynqmon#292). TARGETOS/TARGETARCH are auto-populated by BuildKit;
# on a plain `docker build` they resolve to the host platform, so the Makefile
# `docker` target keeps working unchanged.
ARG TARGETOS
ARG TARGETARCH

# Version stamp for `asynqmon --version` and /api/features. The publish
# workflow passes the semver tag; a plain `docker build` gets "devel".
# .git/ is dockerignored, so the linker cannot read vcs.* info; the stamp is
# the only in-binary identity of the image.
ARG VERSION=devel

# Set necessary environmet variables needed for the image and build the server.
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

# Run go build (with ldflags to reduce binary size).
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o asynqmon ./cmd/asynqmon

#
# Third stage:
# Creating and running a new scratch container with the backend binary.
#

FROM scratch

# Root CAs so TLS Redis / HTTPS Prometheus verification works (see above).
COPY --from=backend ["/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/certs/ca-certificates.crt"]

# Copy binary from /build to the root folder of the scratch container.
COPY --from=backend ["/build/asynqmon", "/"]

# Run unprivileged (matches the Helm chart's runAsUser; plain `docker run`
# used to get uid 0). 65534 = nobody.
USER 65534:65534

# Command to run when starting the container.
ENTRYPOINT ["/asynqmon"]
