.PHONY: build run test clean install dist lint vet

# Build variables
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS = -X main.version=$(VERSION)
BINARY = ogc
CMD = ./cmd/ogc
REASONING_BINARY = ogc-reasoning-proxy
REASONING_CMD = ./cmd/ogc-reasoning-proxy

# ── Development ────────────────────────────────────────────────────

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)
	go build -o bin/$(REASONING_BINARY) $(REASONING_CMD)

run:
	go run -ldflags "$(LDFLAGS)" $(CMD)

test:
	go test ./... -v -race

vet:
	go vet ./...

lint: vet test

clean:
	rm -rf bin/ dist/

install: build
	cp bin/$(BINARY) $(GOPATH)/bin/$(BINARY) 2>/dev/null || \
		cp bin/$(BINARY) $(HOME)/go/bin/$(BINARY) 2>/dev/null || \
		go install -ldflags "$(LDFLAGS)" $(CMD)

# ── Release / Cross-Compilation ────────────────────────────────────

PLATFORMS = \
	darwin-amd64 \
	darwin-arm64 \
	linux-amd64 \
	linux-arm64 \
	windows-amd64 \
	windows-arm64

RELEASE_LDFLAGS = $(LDFLAGS) -s -w

dist: clean
	@mkdir -p dist
	@echo "Building release binaries (version: $(VERSION))..."
	@for platform in $(PLATFORMS); do \
		IFS='-' read -r GOOS GOARCH <<< "$$platform"; \
		EXT=""; \
		[ "$$GOOS" = "windows" ] && EXT=".exe"; \
		echo "  → $$GOOS/$$GOARCH"; \
		CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH \
			go build -ldflags "$(RELEASE_LDFLAGS)" \
				-o "dist/$(BINARY)_$${platform}$${EXT}" \
				$(CMD); \
		CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH \
			go build -trimpath -ldflags "-s -w" \
				-o "dist/$(REASONING_BINARY)_$${platform}$${EXT}" \
				$(REASONING_CMD); \
	done
	@echo ""
	@echo "Generating checksums..."
	@cd dist && sha256sum $(BINARY)_* > checksums.txt
	@echo ""
	@echo "Built binaries:"
	@ls -lh dist/
