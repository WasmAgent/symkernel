# SK-2: pinned to the version in the org certified stack
# (aep-certified-2026-09-13-03 / @wasmagent/protocol 0.1.10).
PROTOCOL_VERSION := 0.1.10
PROTOCOL_PACKAGE := @wasmagent/protocol@$(PROTOCOL_VERSION)
# SK-1: repo root — relative paths used to resolve inside the mktemp dir,
# so sync-schemas wrote into a directory that was deleted on exit.
ROOT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
SCHEMA_FILES := constraint-ir.schema.json constraint-violation.schema.json

# ---- CGO / Z3 linker flags ----
# go-z3 uses CGO to bind the Z3 SMT solver shared library.
# The go-z3 package embeds #cgo LDFLAGS: -lz3, which tells the
# Go linker to link against libz3.  At build time the -dev package
# provides headers and the linker stub; at runtime libz3.so must be
# present (or rpath / LD_LIBRARY_PATH must point to it).
#
# Inside Docker (the repo-root Dockerfile): CGO_ENABLED=1 and libz3-dev
# are set in the builder stage; libz3 runtime is installed in the
# final stage.
#
# Local development (apt-based):
#   apt-get install libz3-dev
#   CGO_ENABLED=1 go build ./cmd/symkerneld
#
# Key environment variables:
#   CGO_ENABLED=1            Required — go-z3 is a CGO package.
#   PKG_CONFIG_PATH           If libz3 is installed to a non-standard prefix,
#                             pkg-config must find z3.pc for correct -I/-L flags.

CGO_ENABLED ?= 1
export CGO_ENABLED

BINARY    := symkerneld
BUILD_DIR := ./cmd/symkerneld

.PHONY: build sync-schemas check-schemas

build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) $(BUILD_DIR)

sync-schemas:
	@set -eu; \
	tmpdir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	npm pack $(PROTOCOL_PACKAGE) --silent --pack-destination "$$tmpdir" > /dev/null; \
	tgz="$$tmpdir"/wasmagent-protocol-$(PROTOCOL_VERSION).tgz; \
	node scripts/verify-protocol-lock.mjs "$$tgz"; \
	tar -xzf "$$tgz" -C "$$tmpdir"; \
	mkdir -p "$(ROOT_DIR)/schemas"; \
	for file in $(SCHEMA_FILES); do \
		cp "$$tmpdir/package/schemas/compliance/$$file" "$(ROOT_DIR)/schemas/$$file"; \
	done

check-schemas:
	@set -eu; \
	tmpdir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	npm pack $(PROTOCOL_PACKAGE) --silent --pack-destination "$$tmpdir" > /dev/null; \
	tgz="$$tmpdir"/wasmagent-protocol-$(PROTOCOL_VERSION).tgz; \
	node scripts/verify-protocol-lock.mjs "$$tgz"; \
	tar -xzf "$$tgz" -C "$$tmpdir"; \
	for file in $(SCHEMA_FILES); do \
		if ! cmp -s "$(ROOT_DIR)/schemas/$$file" "$$tmpdir/package/schemas/compliance/$$file"; then \
			echo "schemas/$$file drifted from $(PROTOCOL_PACKAGE)"; \
			diff -u "$$tmpdir/package/schemas/compliance/$$file" "$(ROOT_DIR)/schemas/$$file" || true; \
			exit 1; \
		fi; \
	done
