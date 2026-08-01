# syntax=docker/dockerfile:1.7

# Build the Z3 CLI separately so the final image does not need a compiler,
# package manager, or the Z3 development headers.
FROM debian:bookworm-slim AS z3-builder

ARG BUILD_Z3_FROM_SOURCE=0
ARG Z3_VERSION=4.13.4
ENV BUILD_Z3_FROM_SOURCE=${BUILD_Z3_FROM_SOURCE} \
    Z3_VERSION=${Z3_VERSION}

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        cmake \
        g++ \
        git \
        make \
        python3; \
    install -d /opt/z3/bin /opt/z3/lib; \
    case "${BUILD_Z3_FROM_SOURCE}" in \
        1|true|TRUE|yes|YES) \
            git clone --depth 1 --branch "z3-${Z3_VERSION}" \
                https://github.com/Z3Prover/z3.git /tmp/z3; \
            cmake -S /tmp/z3 -B /tmp/z3-build \
                -DCMAKE_BUILD_TYPE=Release \
                -DZ3_BUILD_EXECUTABLE=ON \
                -DZ3_BUILD_LIBZ3_SHARED=OFF; \
            cmake --build /tmp/z3-build --parallel; \
            install -D /tmp/z3-build/z3 /opt/z3/bin/z3; \
            find /tmp/z3-build -type f -name 'libz3.a' \
                -exec install -D {} /opt/z3/lib/libz3.a \;; \
            ;; \
        *) \
            apt-get install -y --no-install-recommends z3 libz3-dev; \
            install -D /usr/bin/z3 /opt/z3/bin/z3; \
            find /usr/lib -type f -name 'libz3.so.*' \
                -exec install -D {} /opt/z3/lib/ \;; \
            ;; \
    esac; \
    rm -rf /tmp/z3 /tmp/z3-build /var/lib/apt/lists/*

# CGO is enabled explicitly because the Z3-enabled application is intended to
# be compatible with the CGO build path, even though the current process
# wrapper invokes the z3 executable rather than linking its C API directly.
FROM golang:1.25-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /symkerneld ./cmd/symkerneld

# distroless/base supplies the dynamic loader and standard C++ runtime needed
# by the distro-package path. Source builds link libz3 statically into z3 and
# therefore only need the executable in that mode.
FROM gcr.io/distroless/base-debian12:nonroot AS final

COPY --from=go-builder /symkerneld /usr/local/bin/symkerneld
COPY --from=z3-builder /opt/z3/bin/z3 /usr/local/bin/z3
COPY --from=z3-builder /opt/z3/lib/ /usr/local/lib/

ENV SYMKERNEL_ADDR=:8080 \
    LD_LIBRARY_PATH=/usr/local/lib
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/symkerneld"]
