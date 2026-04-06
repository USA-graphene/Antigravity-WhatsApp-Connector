.PHONY: build run clean test

# Build the connector binary
build:
	@echo "🔨 Building..."
	go build -o bin/connector ./cmd/connector
	@echo "✅ Built: bin/connector"

# Run the connector
run: build
	@echo "🚀 Starting Antigravity WhatsApp Connector..."
	./bin/connector

# Run without building (for development)
dev:
	go run ./cmd/connector

# Clean build artifacts
clean:
	rm -rf bin/
	@echo "🧹 Cleaned"

# Run tests
test:
	go test ./... -v

# Download dependencies
deps:
	go mod tidy
	go mod download
	@echo "✅ Dependencies downloaded"

# Setup: create config from example
setup:
	@if [ ! -f config.yaml ]; then \
		cp config.example.yaml config.yaml; \
		echo "✅ Created config.yaml from example."; \
		echo "   Edit config.yaml with your settings."; \
	else \
		echo "⚠️  config.yaml already exists."; \
	fi

# Lint
lint:
	golangci-lint run ./...
