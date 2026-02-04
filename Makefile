# GoGrowatt Makefile

BINARY_NAME := growatt-export
BUILD_DIR := bin
GO := go
GOFLAGS := -v
LDFLAGS := -s -w

.PHONY: help build test clean install cover lint fmt vet run

# Default target - print help
help:
	@echo "GoGrowatt - Growatt API Client"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build    Build the growatt-export binary"
	@echo "  test     Run all tests"
	@echo "  cover    Run tests with coverage report"
	@echo "  clean    Remove build artifacts"
	@echo "  install  Install binary to GOPATH/bin"
	@echo "  fmt      Format code with gofmt"
	@echo "  vet      Run go vet"
	@echo "  lint     Run fmt and vet"
	@echo "  run      Build and run with sample args (requires PLANT_ID env)"
	@echo ""
	@echo "Examples:"
	@echo "  make build"
	@echo "  make test"
	@echo "  PLANT_ID=12345 make run"

# Build the binary
build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/growatt-export

# Run all tests
test:
	$(GO) test ./... -v

# Run tests with coverage
cover:
	$(GO) test ./... -cover -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out
	@rm -f coverage.out

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -f coverage.out
	$(GO) clean -cache -testcache

# Install to GOPATH/bin
install:
	$(GO) install ./cmd/growatt-export

# Format code
fmt:
	$(GO) fmt ./...

# Run go vet
vet:
	$(GO) vet ./...

# Lint (fmt + vet)
lint: fmt vet

# Run the binary (requires GROWATT_API_KEY and PLANT_ID env vars)
run: build
	@if [ -z "$(PLANT_ID)" ]; then \
		echo "Error: PLANT_ID environment variable required"; \
		echo "Usage: PLANT_ID=12345 make run"; \
		exit 1; \
	fi
	$(BUILD_DIR)/$(BINARY_NAME) --plant-id=$(PLANT_ID) today
