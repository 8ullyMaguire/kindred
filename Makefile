# kindred — build, test, and the memory budget gate.
#
# `make budget` is not a formality. It boots the real server against the
# real corpus, walks the routes, and exits non-zero over the cap from
# SPEC §6. The whole project is a memory budget, and a budget nobody
# checks is a wish.

GO       ?= go
BIN      := bin/kindred
PKG      := ./...
# Cross-compile for the deployment hosts; CGO off keeps the binary static.
GOOS_TARGET ?= linux
GOARCH_TARGET ?= amd64
LDFLAGS  := -s -w

CORPUS   ?= $(HOME)/kindling-data/ao3_metadata.db
DB       ?= $(HOME)/.local/share/kindred/kindred.db
PORT     ?= 8010
MODE     ?= full

.PHONY: all
all: build test

.PHONY: build
build:
	$(GO) build ./...
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kindred
	@echo "built $(BIN) ($$(stat -c%s $(BIN) | numfmt --to=iec))"

.PHONY: test
test:
	$(GO) test -count=1 $(PKG)

.PHONY: race
race:
	$(GO) test -count=1 -race $(PKG)

# The browser suite. Separate from `test` because it needs Node, Playwright and
# a Chromium download, and a developer running `make test` offline should not be
# blocked by that.
.PHONY: e2e
e2e: e2e-install
	cd e2e && npx playwright test

# Installs the Playwright package and its browser. `--with-deps` needs root, so
# it is deliberately NOT here: a CI image with the libs already present is the
# supported path, and a developer who lacks them gets Chromium's own error
# naming the missing library rather than a sudo prompt from make.
.PHONY: e2e-install
e2e-install:
	cd e2e && npm install --no-audit --no-fund

.PHONY: e2e-report
e2e-report:
	@echo "cd e2e && npx playwright show-report"

.PHONY: vet
vet:
	$(GO) vet $(PKG)
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt: files above need formatting" && false)

.PHONY: install
install: build
	install -Dm755 $(BIN) $(HOME)/.local/bin/kindred
	@echo "installed to $(HOME)/.local/bin/kindred"

# The index. Takes about 100 seconds against the real corpus, so it is
# not part of `all`.
.PHONY: ingest
ingest: build
	$(BIN) ingest --corpus $(CORPUS) --db $(DB) --mode $(MODE)

# The budget gate. Boots the server, exercises the routes, reads VmHWM
# from the kernel, and fails if the peak is over the cap for the mode.
.PHONY: budget
budget: build
	@./scripts/budget.sh $(BIN) $(CORPUS) $(DB) $(PORT) $(MODE)

# The arena batch timer. Prints what would be installed; the install itself
# needs root, so it is not done for you.
.PHONY: arena-timer
arena-timer:
	@echo "sudo install -m644 deploy/kindred-arena-batch.service /etc/systemd/system/"
	@echo "sudo install -m644 deploy/kindred-arena-batch.timer   /etc/systemd/system/"
	@echo "sudo systemctl daemon-reload"
	@echo "sudo systemctl enable --now kindred-arena-batch.timer"
	@echo "systemctl list-timers kindred-arena-batch.timer"

.PHONY: clean
clean:
	rm -rf bin
	$(GO) clean -testcache
