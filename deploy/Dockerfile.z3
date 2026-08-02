# syntax=docker/dockerfile:1
# ============================================================
# symkernel — extended multi-stage Z3 Dockerfile
# ============================================================
# Variant of the repo-root Dockerfile for the Z3 verification tier
# (POST /v1/verify/z3, /v1/verify/smt, /v1/verify/symbolic). It adds a
# dedicated Z3 library compilation stage:
#
#   * BUILD_Z3_FROM_SOURCE=1 compiles Z3 from a pinned source release
#     (Z3_VERSION) with -DBUILD_LIBZ3_SHARED=OFF, producing a fully
#     static z3 CLI binary and a static libz3.a.  symkerneld is then
#     linked with the external static linker (netgo + -static), so the
#     resulting image runs on gcr.io/distroless/static.
#   * When BUILD_Z3_FROM_SOURCE is unset (or 0), the build installs
#     Debian's z3/libz3-dev packages and targets debian:bookworm-slim,
#     matching the repo-root Dockerfile behavior.
#
# The z3 CLI binary must be present at runtime because the solver is
# invoked via exec.Command("z3", "-in") in internal/verify/z3.go,
# internal/smt/smt.go, and internal/z3/z3.go.  This Dockerfile bakes it
# into the final image so the image runs standalone (no docker-compose
# sidecar needed).
#
# Build knobs:
#   BUILD_Z3_FROM_SOURCE  "1" -> compile Z3 from source (static).
#   Z3_VERSION            Pinned Z3 release tag, e.g. "4.13.4".
#   Z3_SHA256             Optional release tarball SHA-256 (verified).
#   FINAL_IMAGE           Final base image.  Default debian:bookworm-slim;
#                         use gcr.io/distroless/static together with
#                         BUILD_Z3_FROM_SOURCE=1.
#
# Usage:
#   # Apt z3, debian-slim final image (default):
#   docker build -f deploy/Dockerfile.z3 -t symkernel:z3 .
#
#   # Pinned source build, statically linked, distroless final image:
#   docker build -f deploy/Dockerfile.z3 \
#     --build-arg BUILD_Z3_FROM_SOURCE=1 \
#     --build-arg Z3_VERSION=4.13.4 \
#     --build-arg FINAL_IMAGE=gcr.io/distroless/static \
#     -t symkernel:z3-distroless .
# ============================================================

ARG BUILD_Z3_FROM_SOURCE=0
ARG Z3_VERSION=4.13.4
ARG Z3_SHA256=
ARG FINAL_IMAGE=debian:bookworm-slim

# ---- Z3 library compilation stage ----
# Produces a uniform /opt/z3 layout consumed by later stages:
#   /opt/z3/bin/z3           z3 CLI binary
#   /opt/z3/include/z3*.h    Z3 headers (CGO bindings)
#   /opt/z3/lib/libz3.a      static library (source build)
#   /opt/z3/lib/libz3.so*    shared library + runtime libs (apt fallback)
# When BUILD_Z3_FROM_SOURCE=1 the release tarball for the pinned
# Z3_VERSION is downloaded, its checksum verified when Z3_SHA256 is
# given, and the library is compiled with a static, position-independent
# libz3.a and a fully static z3 executable (-static for distroless).
FROM debian:bookworm AS z3
ARG BUILD_Z3_FROM_SOURCE=0
ARG Z3_VERSION=4.13.4
ARG Z3_SHA256=
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    if [ "${BUILD_Z3_FROM_SOURCE}" = "1" ]; then \
        apt-get install -y --no-install-recommends build-essential cmake libc6-dev python3; \
        curl -fsSL -o /tmp/z3.tar.gz \
            "https://github.com/Z3Prover/z3/archive/refs/tags/z3-${Z3_VERSION}.tar.gz"; \
        if [ -n "${Z3_SHA256}" ]; then \
            echo "${Z3_SHA256}  /tmp/z3.tar.gz" | sha256sum -c -; \
        fi; \
        mkdir -p /tmp/z3-src; \
        tar -xzf /tmp/z3.tar.gz -C /tmp/z3-src --strip-components=1; \
        cmake -S /tmp/z3-src -B /tmp/z3-build \
            -DCMAKE_BUILD_TYPE=Release \
            -DCMAKE_INSTALL_PREFIX=/opt/z3 \
            -DBUILD_LIBZ3_SHARED=OFF \
            -DBUILD_LIBZ3_STATIC=ON \
            -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
            -DCMAKE_EXE_LINKER_FLAGS="-static -static-libstdc++ -static-libgcc"; \
        cmake --build /tmp/z3-build -j"$(nproc)"; \
        cmake --install /tmp/z3-build; \
    else \
        apt-get install -y --no-install-recommends z3 libz3-dev; \
        mkdir -p /opt/z3/bin /opt/z3/include /opt/z3/lib; \
        install -m 0755 "$(command -v z3)" /opt/z3/bin/z3; \
        cp /usr/include/z3*.h /opt/z3/include/; \
        cp -a /usr/lib/x86_64-linux-gnu/libz3* /opt/z3/lib/; \
        cp -a /usr/lib/x86_64-linux-gnu/libstdc++.so.6* /opt/z3/lib/; \
        cp -a /usr/lib/x86_64-linux-gnu/libgcc_s.so.1 /opt/z3/lib/; \
    fi; \
    rm -rf /var/lib/apt/lists/* /tmp/z3.tar.gz /tmp/z3-src /tmp/z3-build

# ---- Go builder ----
FROM golang:1.25-bookworm AS builder
ARG BUILD_Z3_FROM_SOURCE=0

# Uniform Z3 layout from the compilation stage (headers, static/shared
# library, and the z3 CLI used at runtime).
COPY --from=z3 /opt/z3 /opt/z3
ENV Z3_DIR=/opt/z3

# libc6-dev provides libc.a for -static external linking; the official
# golang image ships gcc.
RUN apt-get update && \
    apt-get install -y --no-install-recommends libc6-dev && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Source build (BUILD_Z3_FROM_SOURCE=1): CGO is enabled so a go-z3 CGO
# binding would link against libz3.a (statically linking Z3 into the
# binary); -tags netgo forces the pure-Go net resolver so the external
# static link does not drag in glibc NSS modules, and -linkmode
# external -extldflags "-static -static-libstdc++ -static-libgcc"
# produces a fully static symkerneld binary for gcr.io/distroless/static.
#
# Apt fallback (default): CGO enabled to match the repo-root Dockerfile;
# the pure-Go binary stays static and the final image provides glibc for
# Debian's dynamically linked z3 CLI.
RUN set -eu; \
    if [ "${BUILD_Z3_FROM_SOURCE}" = "1" ]; then \
        CGO_ENABLED=1 \
        CGO_CFLAGS="-I${Z3_DIR}/include" \
        CGO_LDFLAGS="-L${Z3_DIR}/lib -lz3" \
        go build -trimpath \
            -tags netgo \
            -ldflags="-s -w -linkmode external -extldflags '-static -static-libstdc++ -static-libgcc'" \
            -o /symkerneld ./cmd/symkerneld; \
    else \
        CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /symkerneld ./cmd/symkerneld; \
    fi

# ---- Final image ----
# debian:bookworm-slim by default (apt z3 mode).  Pass
# FINAL_IMAGE=gcr.io/distroless/static together with
# BUILD_Z3_FROM_SOURCE=1 for a fully static, distroless-compatible
# image.  No RUN instructions are used here so distroless/static (which
# ships no shell) works unchanged.
FROM ${FINAL_IMAGE} AS final

COPY --from=builder /symkerneld /usr/local/bin/symkerneld
COPY --from=z3 /opt/z3/bin/z3 /usr/local/bin/z3
# The apt z3 CLI is dynamically linked against libz3.so.1/libstdc++.so.6;
# carrying them next to the binary keeps the same final-image layout
# working on debian-slim.  Statically linked source builds ignore this.
COPY --from=z3 /opt/z3/lib /opt/z3/lib

ENV PATH="/usr/local/bin:${PATH}" \
    LD_LIBRARY_PATH="/opt/z3/lib:${LD_LIBRARY_PATH}" \
    SYMKERNEL_ADDR=:8080

EXPOSE 8080
ENTRYPOINT ["symkerneld"]
