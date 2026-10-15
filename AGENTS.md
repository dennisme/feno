# AGENTS.md

Guidance for coding agents working in this repository.

## What this is

`feno` compiles anomaly detector specifications into the alert rules a stack
already evaluates, and replays them over real history before they ship. The
specification is OpenSLO's, extended rather than replaced. The output is
ordinary recording and alerting rules, so there is no daemon here and no
anomaly score to take on trust.

Read [README.md](README.md) for the front door, `docs/` for the user-facing
manual, and [spec.md](spec.md) for the design and the research behind it. The
spec is the source of truth for intended behaviour. If code and spec disagree,
say so rather than picking one silently.

The sibling project is
[clickhouse-ruler](https://github.com/dennisme/clickhouse-ruler), which
evaluates alert rules written as ClickHouse SQL. feno generates rules for it.
The dependency runs one way: feno may import the ruler's rule types, and the
ruler never imports feno. Putting specification machinery inside an evaluator
that sits in the alert path is the thing that direction exists to prevent.

## Commands

Everything goes through [`just`](https://github.com/casey/just). Run `just` on
its own to list recipes.

```bash
just check            # what CI runs: lint, build, unit tests, promtool, markdownlint
just test             # unit tests with -race
just lint             # golangci-lint, formatting checked via `fmt --diff`
just fix              # apply every fix golangci-lint can make
just build            # go build ./...
just coverage         # coverage across the tree
just promtool         # promtool check rules over every generated rule file
just smoketest        # the capability probe against two real Prometheus servers
just backtest-history # suggest, generate and backtest against a month of real history
just markdownlint     # markdownlint-cli2 over the docs
just docs-serve       # preview the documentation site locally
just init             # mise tool versions and pre-commit hooks
just release-snapshot # build release archives locally, no tag needed
```

Run `just check` before claiming work is done. If you touched Markdown, that
includes `just markdownlint`, which CI runs too.

```bash
go run ./cmd/feno version
go run ./cmd/feno backtest -f detectors/ --url http://localhost:9090 --from -30d
```

`version` prints the release tag, the commit and whether the tree was dirty.
The tag comes from `-ldflags "-X .../internal/buildinfo.Version=<tag>"` at
release time and reads `dev` otherwise; the rest comes from the toolchain.

`suggest` drafts a detector per SLI and writes it out for review. It profiles
each SLI's series over a stated window and decides seasonality, strategy and
class from what it measured, leaving the fields no profile can measure absent
so internal/spec's defaults apply. `--url` is required: there is no offline
draft, because a field nothing measured is a field nobody should trust. A
ratio SLI contributes its `total` rather than a composed ratio, and a series
no band suits is refused with the measurement behind it. A run that drafted
nothing exits 1 and an unreachable datasource exits 3. Drafts go to stdout
unless `-o` names a directory, and a file already there is refused by name
unless `--force`.

```bash
go run ./cmd/feno suggest --from-sli slis/ -f specs/ --url http://localhost:9090 -o detectors/
```

`--from-sli` says which SLIs to draft from and `-f` is the tree a drafted
`sloRef` is resolved against, which defaults to the `--from-sli` paths. Both
mean a tree of specifications, as they do in every command here: a directory
holding generated rule files alongside them is refused as an unknown kind, so
point them at the OpenSLO half of the repository rather than at its root.

`backtest` reads history and prints the report. It writes nothing, needs only
a range query endpoint, and exits 1 when a detector is over the noise budget
in `feno.yaml` so it can gate a pull request. A datasource it cannot reach
exits 3, which is a check that did not run rather than one that passed.
`--from` takes OpenSLO's duration shorthand, so `-30d` and `-1w` work where
Go's own parser has no unit for either.

The budget is a file and not a flag, and `internal/config` holds the reason at
length. Each detector is held to the strictest limit in the files from the
`-f` path down to its own directory, so a team may tighten what the repository
set and may not loosen it. A finding names the file that set the limit it
broke. `-config` names one file and replaces the walk.

The tests that need a real Prometheus are behind the `integration` tag and
skip with a message naming the environment variables they want.

```bash
FENO_PROMETHEUS_URL=http://localhost:9090 go test -tags integration ./internal/backtest/
```

The budget overlap's integration test wants two more, `FENO_SLO_GOOD_QUERY`
and `FENO_SLO_TOTAL_QUERY`, which are the two halves of an SLO ratio over the
same server. `cmd/feno`'s integration test wants the same two and runs the
whole loop, drafting a detector from an SLI built out of them and then
generating and backtesting what it wrote. `just backtest-history` passes all
of them against the generated month, to internal/backtest, internal/suggest
and cmd/feno.

## Layout

What exists:

| Path | What lives there |
| --- | --- |
| `cmd/feno` | CLI. Command dispatch and exit codes |
| `internal/buildinfo` | What a binary is: version, commit, build time |
| `internal/spec` | The `AnomalyDetector` kind, the loader over a walked tree of mixed OpenSLO and detector documents, and validation. Resolves both of a detector's references, `metricSourceRef` to a `DataSource` and `sloRef` to an `SLO` with its indicator followed. Imports the OpenSLO go-sdk for its kinds rather than restating them |
| `internal/prometheus` | Range-query reader over `/api/v1/query_range`, chunked and stitched |
| `internal/detect` | The band representation. One implementation, compiled by every target and evaluated by the backtest, because a second one makes the backtest stop predicting production |
| `internal/rules` | The Prometheus target. Compiles a band into recording and alerting rules, compiles a linked SLO's objective into the burn query the budget overlap is read from, and declares what the target cannot express |
| `internal/episode` | Band breaches grouped into the episodes a person would have been paged for, matching how Prometheus runs `for`, and the intersection of those episodes with another timeline |
| `internal/config` | `feno.yaml`: the repository's targets and its noise budget, resolved per detector from the files above it with the stricter limit binding. A limit is a pointer, because zero is a budget somebody means |
| `internal/suggest` | `suggest`: one profiling read through internal/prometheus, the decisions as a pure function over that profile, and the drafted document. Deterministic and offline once the read is done, and nothing in it calls a model |
| `internal/backtest` | Replay tying the reader, the band and the episode report together: the query range a lookback demands, the second read the budget overlap intersects with, the aggregation across one query's series, and the report a person reads |
| `smoketest/` | Two Prometheus servers, one started with the experimental functions and one without, for the capability probe |
| `history/` | A Prometheus over a generated month of TSDB blocks, for the backtest's read against a range past the resolution limit |
| `docs/` | The published site |
| `spec.md` | Design, research, and the decisions already settled |

Every package the specification calls for now exists. What is missing is the
ClickHouse target. Do not create a package as an empty directory ahead of the
work that fills it.

## Conventions

- Go 1.26. Module `github.com/dennisme/feno`. Standard library first. Adding a
  dependency is a decision to raise, not to make. The OpenSLO go-sdk and a
  YAML parser are the two that are already agreed.
- `golangci-lint` v2 with `misspell`, `unconvert`, `unparam`, `gosec`, plus
  `gofmt` and `goimports` as formatters. `gosec` is excluded in `_test.go`
  because fixtures and fake credentials trip G304 and G101.
- Comments explain why a thing is the way it is, often at length, and the
  `justfile` does the same. Match that. Never write a comment about what the
  code used to do or that something changed.
- Band math belongs in one place and is compiled per dialect, never written as
  target-specific query strings in two emitters.
- Every target declares what it cannot express. A specification that degrades
  on the chosen target says so on generate rather than quietly emitting an
  approximation.
- Nothing in the generation or evaluation path may call out to a model. Typed
  decision models are an opt-in authoring and triage aid, live in their own
  module, and write their answers back into the specification, which is then
  committed and is the only source of truth.

## Testing

- Tests are table-driven with fixtures in each package's `testdata/`. Add a
  fixture rather than embedding a large YAML string in the test.
- Generated rules are tested against golden files, and the first golden files
  come from an existing proven rule library rather than from this tool's own
  output.
- No mocked data sources. Anything that needs a real Prometheus or a real
  ClickHouse is an integration test against a real one, behind a build tag.
- Test output must be clean. If a test expects an error, capture and assert the
  error output rather than letting it print.

## Commits

Conventional Commits, enforced by commitizen (`.cz.toml`) via a `commit-msg`
hook and a CI check. Allowed types: `feat`, `fix`, `doc`, `perf`, `ref`,
`test`, `chore`, `ci`, `revert`. Format:

```text
<type>(<optional scope>): <subject>
```

Never bypass a pre-commit hook. Do not commit unless asked to.

## Status

`feno version`, `feno suggest`, `feno generate` and `feno backtest` run. An
SLI already in the repository drafts into a detector document, which loads
into a `detect.Spec`, compiles into a Prometheus rule file, and replays over
history into a report that is held to the budget in `feno.yaml` and says how
much of its paging coincided with the error budget of the SLO its `sloRef`
names; what is missing is the ClickHouse target. The README `Status` and
`Build order` sections say what is next; keep them accurate when you land
something they describe.
