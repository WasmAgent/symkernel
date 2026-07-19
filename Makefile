SCHEMA_UPSTREAM_REPO := https://github.com/WasmAgent/wasmagent-js.git
SCHEMA_UPSTREAM_COMMIT := 51a0b036efeb9189123aa46d358a5ed18ea7824c
SCHEMA_UPSTREAM_DIR := packages/compliance/schemas
SCHEMA_FILES := constraint-ir.schema.json constraint-violation.schema.json

.PHONY: sync-schemas check-schemas

sync-schemas:
	@set -eu; \
	tmpdir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	git clone --quiet --filter=blob:none --no-checkout "$(SCHEMA_UPSTREAM_REPO)" "$$tmpdir/wasmagent-js"; \
	git -C "$$tmpdir/wasmagent-js" checkout --quiet "$(SCHEMA_UPSTREAM_COMMIT)" -- "$(SCHEMA_UPSTREAM_DIR)"; \
	mkdir -p schemas; \
	for file in $(SCHEMA_FILES); do \
		cp "$$tmpdir/wasmagent-js/$(SCHEMA_UPSTREAM_DIR)/$$file" "schemas/$$file"; \
	done

check-schemas:
	@set -eu; \
	tmpdir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	git clone --quiet --filter=blob:none --no-checkout "$(SCHEMA_UPSTREAM_REPO)" "$$tmpdir/wasmagent-js"; \
	git -C "$$tmpdir/wasmagent-js" checkout --quiet "$(SCHEMA_UPSTREAM_COMMIT)" -- "$(SCHEMA_UPSTREAM_DIR)"; \
	for file in $(SCHEMA_FILES); do \
		if ! cmp -s "schemas/$$file" "$$tmpdir/wasmagent-js/$(SCHEMA_UPSTREAM_DIR)/$$file"; then \
			echo "schemas/$$file drifted from $(SCHEMA_UPSTREAM_REPO) at $(SCHEMA_UPSTREAM_COMMIT)"; \
			diff -u "$$tmpdir/wasmagent-js/$(SCHEMA_UPSTREAM_DIR)/$$file" "schemas/$$file" || true; \
			exit 1; \
		fi; \
	done
