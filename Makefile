BIN := bin/pony
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build run test fmt fmt-check tidy-check vet check clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/pony

run:
	go run -ldflags "$(LDFLAGS)" ./cmd/pony

test:
	go test -race ./...

fmt:
	gofmt -w .

fmt-check:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

tidy-check:
	test -z "$$(go mod tidy -diff)"

vet:
	go vet ./...

check: fmt-check tidy-check vet test

clean:
	rm -rf $(BIN)
