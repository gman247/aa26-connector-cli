BIN := bin/aa26-connector
PREFIX ?= /usr/local

.PHONY: build install uninstall test clean

# Build a single static binary. Schema is embedded at build time so the
# resulting binary works anywhere with no external files.
build:
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
	go test ./...

# Run the CLI's three side-effect-free subcommands against a fresh scaffold
# to catch regressions in the build pipeline.
smoke: build
	@tmp=$$(mktemp -d) && \
	  $(CURDIR)/$(BIN) new smoke-check --lang=bash --dir="$$tmp/c" >/dev/null && \
	  $(CURDIR)/$(BIN) validate "$$tmp/c/connector.yaml" && \
	  rm -rf "$$tmp" && \
	  echo "✓ smoke ok"

clean:
	rm -rf bin
