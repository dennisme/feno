# feno

Anomaly detector specifications, written as OpenSLO-shaped YAML in git,
compiled into the alert rules your stack already evaluates, and replayed over
history before they ship.

The name is for phenology, the study of when recurring natural events happen.
Seasonality is the whole problem: a metric is not anomalous because it is
high, it is anomalous because it is high for a Tuesday at 03:00.

## Status: both commands run against Prometheus

`feno backtest` replays a detector over real history and prints the report.
It needs a URL and read access, writes nothing, and exits non-zero when a
threshold flag is exceeded, so it can gate a pull request.

```bash
feno backtest -f detectors/ --url http://mimir/prometheus --from -30d
```

`feno generate` compiles each specification into a rule file, one file per
specification, carrying a provenance header naming the tool version, the
specification and a hash of it. `--check` writes nothing and fails when the
committed files are not what the specifications compile to, which is the
drift gate a pull request runs offline.

`--url` probes that server once for the optional functions it was started
with, so the band is compiled against what the server has rather than against
a flag somebody had to know the answer to. Without it the compilation is
offline and `--experimental-functions` decides, which is what keeps the drift
gate working on a fork pull request with no path to production.

```bash
feno generate -f detectors/ -o rules/anomaly/
feno generate -f detectors/ -o rules/anomaly/ --check
feno generate -f detectors/ -o rules/anomaly/ --url http://mimir/prometheus
```

| Package | What it does |
| --- | --- |
| `internal/spec` | Reads a directory of OpenSLO and detector YAML, resolves each detector's datasource reference, and turns a detector into a band |
| `internal/prometheus` | Reads a range query from anything speaking `/api/v1/query_range`, chunked under the server's resolution limit and stitched back together, and probes one instant query for the optional functions the server has |
| `internal/detect` | The band: one structured representation, every target compiles it and the backtest evaluates it |
| `internal/episode` | Turns band breaches into the episodes a person would have been paged for, matching how Prometheus runs `for` |
| `internal/rules` | Compiles a band into Prometheus recording and alerting rules, and refuses a band PromQL cannot express rather than approximating it |
| `internal/backtest` | Ties the four together: the query range a band's lookback demands, the aggregation across the series one query returns, and the report a person reads |

Specification to band to episodes to report is `backtest`, and specification
to band to rules is `generate`. What is missing is the ClickHouse target.

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
anybody agrees to deploy anything. The budget overlap is the one part not
computed yet; the report names what it is still missing rather than printing a
zero.

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
4. `backtest` over `/api/v1/query_range`, with the episode report
5. `generate`, which is the command over `internal/rules`
6. The ClickHouse target, which depends on four changes in clickhouse-ruler

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
just smoketest # the capability probe against two real Prometheus servers
just init     # mise tool versions and pre-commit hooks
```

```bash
go run ./cmd/feno version
go run ./cmd/feno backtest -f detectors/ --url http://localhost:9090 --from -30d
```

The tests that need a real Prometheus are behind a build tag and say what they
want in their own skip message.

```bash
FENO_PROMETHEUS_URL=http://localhost:9090 go test -tags integration ./internal/backtest/
```

## License

Apache-2.0.
