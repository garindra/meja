GO ?= go
GORELEASER ?= goreleaser

BINARY := meja
MAIN_PACKAGE := .

.PHONY: build test check clean snapshot

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/$(BINARY) $(MAIN_PACKAGE)

test:
	$(GO) test ./...

check:
	test -z "$$(gofmt -l .)"
	$(GO) vet ./...
	$(GO) test ./...
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/$(BINARY) $(MAIN_PACKAGE)

clean:
	rm -rf bin dist

snapshot:
	$(GORELEASER) release --snapshot --clean
