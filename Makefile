BIN := bin/aa26-connector
PREFIX ?= /usr/local

.PHONY: build install uninstall test clean smoke sync-schemas

# Build a single static binary. Schemas are embedded at build time so the
# resulting binary works anywhere with no external files.
build: test
	mkdir -p bin
	go mod tidy
	CGO_ENABLED=0 go build -ldflags='-s -w' -o $(BIN) .
	@echo "✓ built $(BIN) ($$(du -h $(BIN) | cut -f1))"

install: build
	install -m 0755 $(BIN) $(PREFIX)/bin/aa26-connector
	@echo "✓ installed to $(PREFIX)/bin/aa26-connector"

uninstall:
	rm -f $(PREFIX)/bin/aa26-connector

test:
	go vet ./...
	go test ./...

# Run the CLI's three side-effect-free subcommands against a fresh scaffold
# to catch regressions in the build pipeline.
smoke: build
	@tmp=$$(mktemp -d) && \
	  $(CURDIR)/$(BIN) new smoke-check --lang=bash --dir="$$tmp/c" >/dev/null && \
	  $(CURDIR)/$(BIN) validate "$$tmp/c/connector.yaml" && \
	  rm -rf "$$tmp" && \
	  echo "✓ smoke ok"

# Sync the embedded schemas from the upstream connector-prototype monorepo,
# when this CLI lives as a sibling of that tree. No-op on standalone clones.
sync-schemas:
	@if [ -d ../schema ]; then \
		cp ../schema/connector.schema.json schema/connector.schema.json; \
		cp ../schema/finding.schema.json   schema/finding.schema.json; \
		echo "✓ synced schemas from ../schema"; \
	else \
		echo "✗ no ../schema directory — sync-schemas is a no-op outside the connector-prototype monorepo"; \
	fi

clean:
	rm -rf bin
