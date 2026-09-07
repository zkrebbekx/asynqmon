#
# First stage:
# Building a frontend.
#

# Multi-arch build (hibiken/asynqmon#292): both build stages are pinned to the
# BUILD platform so npm/go run natively (no QEMU emulation); Go cross-compiles
# to the TARGET platform via TARGETOS/TARGETARCH below. The frontend bundle is
# architecture-independent.
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend

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

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS backend

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

# Set necessary environmet variables needed for the image and build the server.
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

# Run go build (with ldflags to reduce binary size).
RUN go build -ldflags="-s -w" -o asynqmon ./cmd/asynqmon

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
