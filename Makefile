GO ?= go

.PHONY: build test race vet fmt-check

build:
	$(GO) build -o bin/proxy ./cmd/proxy

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
