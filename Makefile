VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/nution101/orcha/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: build install test vet fmt fmtcheck lint dist clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/orcha ./cmd/orcha

# Local developer install: build into the user-owned home, link into PATH, lay content.
install:
	@mkdir -p $(HOME)/.orcha/bin $(HOME)/.local/bin
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(HOME)/.orcha/bin/orcha ./cmd/orcha
	ln -sf $(HOME)/.orcha/bin/orcha $(HOME)/.local/bin/orcha
	$(HOME)/.orcha/bin/orcha install

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmtcheck:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

lint: vet fmtcheck
	sh scripts/lint-vocab.sh

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  out=dist/orcha-$(VERSION)-$$os-$$arch; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out/orcha ./cmd/orcha; \
	  tar -C $$out -czf $$out.tar.gz orcha; \
	  rm -rf $$out; \
	done
	@cd dist && (command -v sha256sum >/dev/null 2>&1 && sha256sum *.tar.gz || shasum -a 256 *.tar.gz) > checksums.txt
	@echo "dist/ ready"

clean:
	rm -rf bin dist
