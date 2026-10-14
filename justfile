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

# Check every generated rule file with promtool.
#
# spec.md Part 9 counts on this being free verification: the emitter is checked
# by the tool that reads the files, not by a fixture of what it says. The
# version comes from .tool-versions, which moves with the prom/prometheus image
# the smoketest runs.
promtool:
    command -v promtool >/dev/null 2>&1 || { echo "promtool not installed; run 'just init'"; exit 1; }
    promtool check rules --enable-feature=promql-experimental-functions internal/rules/testdata/golden/*.yml
    out=$(mktemp -d); \
    go run ./cmd/feno generate -f cmd/feno/testdata/detectors -o "$out" --experimental-functions >/dev/null; \
    promtool check rules --enable-feature=promql-experimental-functions "$out"/*.yaml; \
    rm -rf "$out"

# Prove the capability probe against a real Prometheus, both ways.
#
# Not part of `check`, because it needs Docker and pulls an image. What it
# covers is the one decision in feno that depends on another project's wording:
# the probe reads a parse error to decide whether mad_over_time may be
# compiled, and a fixture of that sentence is evidence of nothing but itself.
# Two servers, one started with the feature flag and one without, are the only
# thing that says the read is right.
#
# It also runs the binary against both, because the integration test drives a
# package and what a person sees is a process: the line on stderr naming the
# form chosen, the expression that reached the file, and a non-zero exit when
# the server cannot be reached.
smoketest:
    #!/usr/bin/env bash
    set -euo pipefail
    compose="docker compose -f smoketest/compose.yaml"
    trap '$compose down --volumes >/dev/null 2>&1 || true' EXIT
    $compose up -d --wait

    for server in "19090 false quantile_over_time(0.75," "19091 true mad_over_time("; do
        read -r port flag expression <<<"$server"
        url="http://localhost:$port"

        FENO_PROMETHEUS_URL="$url" FENO_EXPECT_EXPERIMENTAL_FUNCTIONS="$flag" \
            env -u GOROOT GOTOOLCHAIN=auto go test -count=1 -tags integration ./internal/prometheus/

        out=$(mktemp -d)
        go run ./cmd/feno generate -f cmd/feno/testdata/detectors -o "$out" --url "$url" >/dev/null
        grep -q -- "$expression" "$out/api-latency.yaml" \
            || { echo "$url: api-latency.yaml does not use $expression"; exit 1; }
        rm -rf "$out"
    done

    # The third answer: a port nothing is listening on is a failure, not a
    # quiet fallback to the band the other server would have got.
    out=$(mktemp -d)
    if go run ./cmd/feno generate -f cmd/feno/testdata/detectors -o "$out" --url http://localhost:19099; then
        echo "generate succeeded against a server that is not there"
        exit 1
    fi
    [ -z "$(ls -A "$out")" ] || { echo "a failed probe wrote files"; exit 1; }
    rm -rf "$out"

    echo "smoketest: both forms compiled from what the server answered"

# Everything CI runs, in the order CI runs it.
check: lint build test promtool markdownlint
