PROTOCOL_PACKAGE := @wasmagent/protocol
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
	cd "$$tmpdir" && npm pack $(PROTOCOL_PACKAGE) --silent > /dev/null; \
	tar -xzf "$$tmpdir"/wasmagent-protocol-*.tgz -C "$$tmpdir"; \
	mkdir -p schemas; \
	for file in $(SCHEMA_FILES); do \
		cp "$$tmpdir/package/schemas/compliance/$$file" "schemas/$$file"; \
	done

check-schemas:
	@set -eu; \
	tmpdir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	cd "$$tmpdir" && npm pack $(PROTOCOL_PACKAGE) --silent > /dev/null; \
	tar -xzf "$$tmpdir"/wasmagent-protocol-*.tgz -C "$$tmpdir"; \
	for file in $(SCHEMA_FILES); do \
		if ! cmp -s "schemas/$$file" "$$tmpdir/package/schemas/compliance/$$file"; then \
			echo "schemas/$$file drifted from $(PROTOCOL_PACKAGE)"; \
			diff -u "$$tmpdir/package/schemas/compliance/$$file" "schemas/$$file" || true; \
			exit 1; \
		fi; \
	done
