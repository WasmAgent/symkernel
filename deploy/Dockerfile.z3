# Dockerfile.z3 — multi-stage build that compiles Z3 from source and
# statically links it into a distroless-compatible symkerneld image.
#
# Usage:
#   docker build -f deploy/Dockerfile.z3 -t symkerneld:z3 .
#
# Optional build args:
#   Z3_VERSION     — Z3 git tag to build (default: z3-4.13.4)
#   BUILD_Z3_FROM_SOURCE — set to "1" to compile Z3; omit to use apt package

ARG GO_VERSION=1.24
ARG Z3_VERSION=z3-4.13.4
ARG BUILD_Z3_FROM_SOURCE=1

# ── Stage 1: Z3 build ────────────────────────────────────────────────────────
FROM debian:bookworm-slim AS z3-builder
ARG Z3_VERSION
ARG BUILD_Z3_FROM_SOURCE

RUN apt-get update && apt-get install -y --no-install-recommends \
    git cmake python3 g++ make ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN if [ "${BUILD_Z3_FROM_SOURCE}" = "1" ]; then \
      git clone --depth 1 --branch "${Z3_VERSION}" \
        https://github.com/Z3Prover/z3.git /z3-src && \
      cmake -S /z3-src -B /z3-build \
        -DCMAKE_BUILD_TYPE=Release \
        -DZ3_BUILD_LIBZ3_SHARED=OFF \
        -DCMAKE_INSTALL_PREFIX=/z3-install && \
      cmake --build /z3-build --parallel $(nproc) && \
      cmake --install /z3-build; \
    else \
      apt-get update && apt-get install -y --no-install-recommends z3 && \
      mkdir -p /z3-install/bin && \
      cp $(which z3) /z3-install/bin/z3; \
    fi

# ── Stage 2: Go build ────────────────────────────────────────────────────────
FROM golang:${GO_VERSION}-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /symkerneld ./cmd/symkerneld

# ── Stage 3: Runtime (distroless) ────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# Z3 binary from stage 1
COPY --from=z3-builder /z3-install/bin/z3 /usr/local/bin/z3

# symkerneld binary from stage 2
COPY --from=go-builder /symkerneld /symkerneld

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/symkerneld"]
