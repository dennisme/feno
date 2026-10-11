# feno

Anomaly detector specifications, written as OpenSLO-shaped YAML in git,
compiled into the alert rules your stack already evaluates, and replayed over
history before they ship.

The name is for phenology, the study of when recurring natural events happen.
Seasonality is the whole problem: a metric is not anomalous because it is
high, it is anomalous because it is high for a Tuesday at 03:00.

## Status: libraries, no commands yet

`feno version` is still the only command that runs. What exists is the
evaluation path underneath `backtest`, built from the bottom up so that each
piece could be tested on its own before anything was wired to a flag.

| Package | What it does |
| --- | --- |
| `internal/prometheus` | Reads a range query from anything speaking `/api/v1/query_range`, chunked under the server's resolution limit and stitched back together |
| `internal/detect` | The band: one structured representation, every target compiles it and the backtest evaluates it |
| `internal/episode` | Turns band breaches into the episodes a person would have been paged for, matching how Prometheus runs `for` |

Reader to band to episodes is the backtest, as a library. What is missing is
the specification loader that would give it something to read and the command
that would run it.

[spec.md](spec.md) is the source of truth for intended behaviour and carries
the research behind it. Read it before proposing a design change.

## The problem

Anomaly detection on metrics is standard practice and barely adopted, and the
algorithms were never the reason. Z-score, MAD, STL decomposition and Random
Cut Forest all work well enough on operational metrics. Tuning them is the
work, and keeping them tuned is the work after that.

Nothing in the open source tool chain answers the question an engineer has
before merging a detector, which is "how often would this have paged me last
month". Prometheus can unit test a rule against fixtures it was handed.
Nothing replays a candidate rule over real history and reports what it would
have done.

## What it will do

Two commands over one specification.

`feno generate` compiles a detector into recording and alerting rules for an
engine you already run. Prometheus, Mimir, Thanos and the stores that speak
PromQL first. ClickHouse, through
[clickhouse-ruler](https://github.com/dennisme/clickhouse-ruler), after that.
No new service, no daemon, no anomaly score to take on trust. The output is
rules a person can read while holding a pager.

`feno backtest` replays a detector over history and reports what it would have
done: separate paging episodes, total firing time, the longest one, which
series caused them, and how much of it coincided with an error budget actually
burning. It writes nothing and needs only read access, so it is useful before
anybody agrees to deploy anything.

## Why the specification is OpenSLO's

Because a team that already keeps SLOs in git has already declared the two
hard things. A `DataSource` says where the metrics live and how to connect. An
`SLI` says which query matters. feno reuses both rather than inventing a
second way to say them, which means the cheapest useful command is deriving a
detector from an SLI that already exists.

Anyone complying with the specification benefits, whatever store is
underneath.

## Build order

1. Specification types, the loader, and a test proving `oslo validate` still
   passes on the OpenSLO half of a mixed directory
2. The band representation, with an explicit model of what each target can and
   cannot express, which is `internal/detect`
3. The Prometheus target, with golden tests against
   [grafana/promql-anomaly-detection](https://github.com/grafana/promql-anomaly-detection)
   and `promtool` in CI
4. `backtest` over `/api/v1/query_range`, with the episode report. The reader
   and the report are in `internal/prometheus` and `internal/episode`; the
   command itself is not written
5. The ClickHouse target, which depends on four changes in clickhouse-ruler

Steps 2 and 4 came before step 1 because both are pure functions over numbers
and a time range, so they could be pinned by tests without a specification
format to argue about first.

Prometheus is first on purpose. It is the restricted dialect: `mad_over_time`
exists but is experimental and off unless the server was started with
`--enable-feature=promql-experimental-functions`, and there is no seasonal
decomposition at all. Building against that forces the capability model to be
honest about the deployment rather than the dialect. ClickHouse has
`seriesDecomposeSTL` and `seriesOutliersDetectTukey` built in and would have
let that stay implicit.

## Development

```bash
just          # list recipes
just check    # what CI runs: lint, build, tests, markdownlint
just test     # unit tests with -race
just init     # mise tool versions and pre-commit hooks
```

```bash
go run ./cmd/feno version
```

## License

Apache-2.0.
