# ============================================================
# symkernel — multi-stage Docker build
# ============================================================
# Phase 0 (M1): static binary, no CGO, gcr.io/distroless/static.
# Phase 1b (M3): CGO enabled for go-z3; libz3.so in final image.
# ============================================================

# ---- Builder ----
FROM golang:1.25-bookworm AS builder

# Z3 development headers and shared library (needed at compile time
# for go-z3 CGO bindings: #cgo LDFLAGS: -lz3).
RUN apt-get update && \
    apt-get install -y --no-install-recommends libz3-dev && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Build with CGO enabled so go-z3 can link against libz3.
# CGO_ENABLED=1 is the default on linux/amd64 but is explicit here
# because some CI/CD pipelines set CGO_ENABLED=0 globally.
ENV CGO_ENABLED=1

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /symkerneld ./cmd/symkerneld

# ---- Final image ----
# Use debian-slim rather than distroless/static because the Z3 SMT
# solver requires a shared library (libz3.so).  distroless/static
# does not ship a dynamic linker.
FROM debian:bookworm-slim AS final

# Runtime Z3: the libz3 shared library AND the z3 CLI binary. The binary
# is required because internal/verify/z3.go invokes Z3 via
# exec.Command("z3", "-in"). On Cloudflare Containers the image runs alone
# (there is no docker-compose sidecar to provide the z3 binary via a shared
# volume, as deploy/docker-compose.yml does for local dev), so the binary
# must be baked into the image for POST /v1/verify/z3 to function.
RUN apt-get update && \
    apt-get install -y --no-install-recommends libz3 z3 && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /symkerneld /usr/local/bin/symkerneld

ENV SYMKERNEL_ADDR=:8080
EXPOSE 8080

ENTRYPOINT ["symkerneld"]
