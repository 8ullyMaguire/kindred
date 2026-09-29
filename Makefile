GO      ?= go
BIN     ?= bin/kindred
PORT    ?= 8010
BUDGET  ?= 262144        # KiB; 256 MiB, the Pi cap (SPEC §6)
CORPUS  ?=
DB      ?=

.PHONY: verify fmt vet test build budget run ingest dump clean

verify: fmt vet test build

fmt:
	@out="$$(gofmt -l . )"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -timeout 300s ./...

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o $(BIN) ./cmd/kindred

# The gate this project exists to satisfy (SPEC §1, §6).
budget: build
	./scripts/budget.sh

run: build
	./$(BIN) serve --db $(DB) --corpus $(CORPUS)

ingest: build
	./$(BIN) ingest --corpus $(CORPUS) --db $(DB)

dump: build
	./$(BIN) dump --corpus $(CORPUS) --db $(DB) --out $(OUT)

clean:
	rm -rf bin
