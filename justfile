# feno development commands. Run `just` to list recipes.

set shell := ["bash", "-euo", "pipefail", "-c"]

# List available recipes.
default:
    @just --list

# Install tool versions (mise) and git hooks.
init:
    command -v mise >/dev/null 2>&1 && mise install || true
    if command -v pre-commit >/dev/null 2>&1; then \
        pre-commit install; \
    else \
        echo "pre-commit not found; install via 'pip install pre-commit' or mise"; \
        false; \
    fi

# Build all packages.
build:
    go build ./...

# Unit tests with the race detector.
test:
    env -u GOROOT GOTOOLCHAIN=auto go test -race ./...

# Coverage across the tree.
#
# -coverpkg=./... is what makes the number mean anything. Without it a package
# is credited only for what its own tests reach, so a package every other one
# reads through reports zero.
coverage:
    env -u GOROOT GOTOOLCHAIN=auto go test -count=1 -coverpkg=./... ./... \
        -coverprofile coverage.out -covermode count
    env -u GOROOT GOTOOLCHAIN=auto go tool cover -html=coverage.out -o coverage.html

# Run golangci-lint, formatters included.
#
# `run` does not apply the formatters configured in .golangci.yml: golangci-lint
# v2 moved those behind `fmt`, which rewrites files rather than reporting on
# them. `--diff` makes it report instead, so unformatted code fails the recipe
# rather than passing it silently. `just fix` is the one that rewrites.
lint:
    env -u GOROOT GOTOOLCHAIN=auto golangci-lint fmt --diff
    env -u GOROOT GOTOOLCHAIN=auto golangci-lint run

# Apply every fix golangci-lint can make, formatting included.
fix:
    env -u GOROOT GOTOOLCHAIN=auto golangci-lint fmt
    env -u GOROOT GOTOOLCHAIN=auto golangci-lint run --fix

# Lint markdown with markdownlint-cli2.
markdownlint:
    npx --yes markdownlint-cli2

# Run pre-commit on all files.
pre-commit:
    pre-commit run --all-files

# Serve the documentation site locally, rebuilding as files change.
#
# Needs the pinned mkdocs-material: `pip install -r requirements-docs.txt`,
# ideally in a virtualenv. Nothing in `just check` needs it, so a contributor
# who never touches the docs never installs Python at all.
docs-serve:
    mkdocs serve

# Build the site the way the Pages workflow does, --strict included, so a
# broken cross-reference fails here rather than after a merge.
docs-build:
    mkdocs build --strict

# Build the release artifacts locally, without a tag.
#
# Same code path the tag workflow runs, so a broken .goreleaser.yaml fails here
# rather than on a tag that cannot be taken back. Writes archives and
# checksums.txt to dist/, which is gitignored.
release-snapshot:
    goreleaser release --snapshot --clean

# Everything CI runs, in the order CI runs it.
check: lint build test markdownlint
