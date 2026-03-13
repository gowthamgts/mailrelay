# Justfile for mailrelay

# Binary name and output directory
binary := "mailrelay"
bin_dir := "bin"
main_pkg := "./cmd/mailrelay"

# Build flags
ldflags := "-s -w"

# Default recipe: show available commands
default:
    @just --list

# Build the binary
build:
    @mkdir -p {{ bin_dir }}
    CGO_ENABLED=0 go build -ldflags="{{ ldflags }}" -o {{ bin_dir }}/{{ binary }} {{ main_pkg }}

# Build for a specific platform (e.g., just build-for linux amd64)
build-for os arch:
    @mkdir -p {{ bin_dir }}
    CGO_ENABLED=0 GOOS={{ os }} GOARCH={{ arch }} go build -ldflags="{{ ldflags }}" -o {{ bin_dir }}/{{ binary }}-{{ os }}-{{ arch }} {{ main_pkg }}

# Clean build artifacts
clean:
    rm -rf {{ bin_dir }}

# Run in development mode with the dev config
dev *args:
    go run {{ main_pkg }} -config config.dev.yaml {{ args }}

# Run in development mode with hot reload (requires: go install github.com/air-verse/air@latest)
dev-watch:
    air

# Send test emails to the running dev server via swaks
dev-test:
    ./scripts/test-emails.sh

# Run the built binary
run *args: build
    ./{{ bin_dir }}/{{ binary }} {{ args }}

# Run tests
test *args:
    go test -v ./... {{ args }}

# Run tests with coverage
test-coverage:
    go test -v -coverprofile=coverage.out ./...
    go tool cover -html=coverage.out -o coverage.html

# Lint the code
lint:
    go vet ./...
    @which staticcheck > /dev/null && staticcheck ./... || echo "staticcheck not installed, skipping"

# Format code
fmt:
    go fmt ./...

# Tidy dependencies
tidy:
    go mod tidy

# Download dependencies
deps:
    go mod download

# Build Docker image
docker-build tag="mailrelay:latest":
    docker build -t {{ tag }} .

# Run in Docker (mounts ./config.yaml into /data and uses a named volume for persistence)
docker-run tag="mailrelay:latest":
    docker run --rm -p 25:25 -p 2623:2623 -v $(pwd)/config.yaml:/data/config.yaml:ro -v mailrelay_data:/data {{ tag }}

# Deploy (customize this for your deployment target)
deploy: build
    @echo "Deploying {{ binary }}..."
    @echo "Configure this recipe for your deployment target (e.g., scp, rsync, kubectl, etc.)"
    @echo "Binary is available at: {{ bin_dir }}/{{ binary }}"

# Build release binaries for common platforms
release:
    @mkdir -p {{ bin_dir }}
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="{{ ldflags }}" -o {{ bin_dir }}/{{ binary }}-linux-amd64 {{ main_pkg }}
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="{{ ldflags }}" -o {{ bin_dir }}/{{ binary }}-linux-arm64 {{ main_pkg }}
    CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="{{ ldflags }}" -o {{ bin_dir }}/{{ binary }}-darwin-amd64 {{ main_pkg }}
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="{{ ldflags }}" -o {{ bin_dir }}/{{ binary }}-darwin-arm64 {{ main_pkg }}

# Check everything (lint, test, build)
check: lint test build
    @echo "All checks passed!"

# --- GoReleaser ---

# Run goreleaser in snapshot mode (no publish)
goreleaser-snapshot:
    goreleaser release --snapshot --clean

# Run goreleaser to create a release (requires GITHUB_TOKEN)
goreleaser-release:
    goreleaser release --clean

# Check goreleaser config
goreleaser-check:
    goreleaser check

# --- Docker Release ---

# Docker image registry
docker_registry := "ghcr.io/gowthamgts"
docker_image := docker_registry + "/mailrelay"

# Build and push multi-arch Docker image for a release
docker-release version:
    docker buildx build \
        --platform linux/amd64,linux/arm64 \
        --tag {{ docker_image }}:{{ version }} \
        --tag {{ docker_image }}:latest \
        --label "org.opencontainers.image.version={{ version }}" \
        --label "org.opencontainers.image.source=https://github.com/gowthamgts/mailrelay" \
        --push \
        .

# Build multi-arch Docker image locally (no push)
docker-release-local version:
    docker buildx build \
        --platform linux/amd64,linux/arm64 \
        --tag {{ docker_image }}:{{ version }} \
        --tag {{ docker_image }}:latest \
        --label "org.opencontainers.image.version={{ version }}" \
        .

# Full release: goreleaser + docker push
release-full version:
    just goreleaser-release
    just docker-release {{ version }}
