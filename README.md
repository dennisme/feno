# feno

Anomaly detector specifications, written as OpenSLO-shaped YAML in git,
compiled into the alert rules your stack already evaluates, and replayed over
history before they ship.

The name is for phenology, the study of when recurring natural events happen.
Seasonality is the whole problem: a metric is not anomalous because it is
high, it is anomalous because it is high for a Tuesday at 03:00.

## Status: both commands run against Prometheus

`feno backtest` replays a detector over real history and prints the report.
It needs a URL and read access, writes nothing, and exits 1 when a detector is
over the noise budget in `feno.yaml`, so it can gate a pull request. A runner
that cannot reach the datasource exits 3 instead, which says the budget was
never checked rather than that it held.

```bash
feno backtest -f detectors/ --url http://mimir/prometheus --from -30d
```

The budget lives in `feno.yaml` beside the specifications, because a limit
somebody can drop by editing the workflow that checks it is not a limit. One
default for the repository, and a detector that needs its own says so in a
line:

```yaml
targets:
  - engine: prometheus
    output: rules/anomaly

budget:
  maxEpisodes: 4
  maxFiringRatio: 0.02
  detectors:
    checkout-request-rate:
      maxEpisodes: 12
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

Each detector is held to the strictest limit in the files above it, from the
path `-f` names down to its own directory, so a repository with a team per
directory gives each team a `feno.yaml` it owns and nobody can loosen the
ceiling above them:

```text
alerting/
  feno.yaml                   # the ceiling. maxEpisodes: 4
  teams/checkout/
    feno.yaml                 # maxEpisodes: 2, and may not say 12
    detectors/
  teams/search/
    feno.yaml
    detectors/
```

```console
$ feno backtest -f alerting/ --url http://mimir/prometheus --from -14d
checkout-request-rate: 10 paging episodes, limit 2 (alerting/teams/checkout/feno.yaml)
search-latency: 10 paging episodes, limit 4 (alerting/feno.yaml)
```

A finding names the file that set the limit it broke, because in a repository
with a budget per team that file is often not the one the author is looking
at. `-f` walks the tree, and `-config` names one file instead and reads
nothing else.

A detector that names the SLO it protects gets the number the report exists
for: how much of its paging coincided with that SLO's error budget actually
being spent.

```yaml
apiVersion: anomaly.openslo.io/v1alpha
kind: AnomalyDetector
metadata:
  name: checkout-request-rate
spec:
  sloRef: checkout-availability    # an SLO in the same tree
  indicator:
    metadata:
      name: checkout-requests
    spec:
      thresholdMetric:
        metricSource:
          metricSourceRef: mimir-prod
          spec:
            query: sum(rate(http_requests_total{service="checkout"}[5m]))
  detection:
    strategy: adaptive
    seasonality: [daily, weekly]
    class: requests
    sensitivity: balanced
  alerting:
    for: 10m
```

```text
  paging episodes       2, plus 0 flaps that crossed the band without reaching for
  paged wall clock      3h  (12.50% of the reported window)
  budget overlap        0 of 2 paging episodes coincided with the budget burning
                        0s of 3h paged wall clock  (0.00%)
  linked SLO            checkout-availability, objective 99.9%
  budget burning        120 of 10081 instants: 0 absent, 0 undefined
```

That report says delete the detector, and it says it without ambiguity: the
budget did burn over the window, and none of the pages landed on it. The burn
signal is compiled from the SLO's own objective and indicator, so nothing has
to be deployed and no recording rule has to be named a particular way. A
detector with no `sloRef` says it has none rather than printing a zero, which
is a different thing.

`-o` is the target's `output` in `feno.yaml` when it is left off, read
relative to the file that declared it.

```bash
feno generate -f detectors/ -o rules/anomaly/
feno generate -f detectors/ -o rules/anomaly/ --check
feno generate -f detectors/ -o rules/anomaly/ --url http://mimir/prometheus
```

| Package | What it does |
| --- | --- |
| `internal/spec` | Reads a directory of OpenSLO and detector YAML, resolves each detector's datasource and SLO references, and turns a detector into a band |
| `internal/prometheus` | Reads a range query from anything speaking `/api/v1/query_range`, chunked under the server's resolution limit and stitched back together, and probes one instant query for the optional functions the server has |
| `internal/detect` | The band: one structured representation, every target compiles it and the backtest evaluates it |
| `internal/episode` | Turns band breaches into the episodes a person would have been paged for, matching how Prometheus runs `for`, and intersects those episodes with another timeline |
| `internal/rules` | Compiles a band into Prometheus recording and alerting rules, and a linked SLO into the burn query the budget overlap is read from, refusing what PromQL cannot express rather than approximating it |
| `internal/config` | Reads `feno.yaml`: the repository's targets and its noise budget, with a limit per detector where one default will not do |
| `internal/backtest` | Ties the others together: the query range a band's lookback demands, the second read the budget overlap is an intersection with, the aggregation across the series one query returns, and the report a person reads |

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
anybody agrees to deploy anything. A detector that links to no SLO is told
apart from one whose pages never landed on a burning budget, because the
second is a finding and the first is a missing line in a file.

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
6. The budget overlap, which is a second read of the same window against the
   burn signal compiled from the SLO a detector links to
7. The ClickHouse target, which depends on four changes in clickhouse-ruler

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
just check    # what CI runs: lint, build, tests, promtool, markdownlint
just test     # unit tests with -race
just smoketest # the capability probe against two real Prometheus servers
just backtest-history # the backtest against a month of generated history
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
