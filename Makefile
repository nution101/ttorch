VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/nution101/ttorch/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: build install test test-fast vet fmt fmtcheck lint dist clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/ttorch ./cmd/ttorch

# Local developer install: build into the user-owned home, link into PATH, lay content.
install:
	@mkdir -p $(HOME)/.ttorch/bin $(HOME)/.local/bin
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(HOME)/.ttorch/bin/ttorch ./cmd/ttorch
	ln -sf $(HOME)/.ttorch/bin/ttorch $(HOME)/.local/bin/ttorch
	$(HOME)/.ttorch/bin/ttorch install

# Full test suite — every test, including the slow internal/orchestrator integration
# (e2e) tests that drive real tmux/git/rebase/validate (~100s). This is the authoritative
# gate: CI runs it on every push/PR (.github/workflows/ci.yml). TESTFLAGS lets CI add
# -race without changing the default local invocation.
test:
	go test $(TESTFLAGS) ./...

# Fast lane — `-short` skips the slow internal/orchestrator integration tests, leaving the
# unit coverage that finishes in seconds. Used by the local trusted gate
# (.ttorch/validate.sh) for quick turnaround. NOT a replacement for `make test`: the full
# suite (incl. those e2e tests) still runs in CI before anything can land, so the gate is
# not weakened.
test-fast:
	go test -short $(TESTFLAGS) ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmtcheck:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

lint: vet fmtcheck

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  out=dist/ttorch-$(VERSION)-$$os-$$arch; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out/ttorch ./cmd/ttorch; \
	  tar -C $$out -czf $$out.tar.gz ttorch; \
	  rm -rf $$out; \
	done
	@cd dist && (command -v sha256sum >/dev/null 2>&1 && sha256sum *.tar.gz || shasum -a 256 *.tar.gz) > checksums.txt
	@echo "dist/ ready"

clean:
	rm -rf bin dist
