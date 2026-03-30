.PHONY: all build-broker build-consumer build-provider build-all build-pi build-pi-armv7 build-linux clean test build-release

# Default target
all: build-all

# Current platform builds
build-broker:
	go build -o bin/httpbroker-broker ./cmd/broker

build-consumer:
	go build -o bin/httpbroker-consumer ./cmd/consumer

build-provider:
	go build -o bin/httpbroker-provider ./cmd/provider

build-all: build-broker build-consumer build-provider

# Cross-compile broker for Raspberry Pi (linux/arm64)
build-pi:
	GOOS=linux GOARCH=arm64 go build -o bin/httpbroker-broker-arm64 ./cmd/broker

# Cross-compile broker for older Raspberry Pi (linux/arm/v7)
build-pi-armv7:
	GOOS=linux GOARCH=arm GOARM=7 go build -o bin/httpbroker-broker-armv7 ./cmd/broker

# Cross-compile all binaries for linux/amd64 (typical VPS/server)
build-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/httpbroker-broker-linux-amd64 ./cmd/broker
	GOOS=linux GOARCH=amd64 go build -o bin/httpbroker-consumer-linux-amd64 ./cmd/consumer
	GOOS=linux GOARCH=amd64 go build -o bin/httpbroker-provider-linux-amd64 ./cmd/provider

# Run tests
test:
	go test ./...

# Clean build artifacts
clean:
	rm -rf bin/

# Build with version info (set VERSION variable: make build-all VERSION=v1.0.0)
VERSION ?= dev
LDFLAGS = -ldflags "-X main.version=$(VERSION)"

build-release:
	go build $(LDFLAGS) -o bin/httpbroker-broker ./cmd/broker
	go build $(LDFLAGS) -o bin/httpbroker-consumer ./cmd/consumer
	go build $(LDFLAGS) -o bin/httpbroker-provider ./cmd/provider
