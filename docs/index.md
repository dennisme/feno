# feno

Anomaly detector specifications, written as OpenSLO-shaped YAML in git,
compiled into the alert rules your stack already evaluates, and replayed over
history before they ship.

!!! warning "Status: three commands run against Prometheus"

    `feno suggest --from-sli slis/ --url <endpoint> -o detectors/` drafts a
    detector from an SLI already in the repository, `feno backtest -f
    detectors/ --url <endpoint> --from -30d` replays it over history and
    prints the report, and `feno generate -f detectors/ -o rules/anomaly/`
    compiles the rule files, with `--check` as the drift gate. The ClickHouse
    target is next. Every field those three read is in
    [Writing a detector](detectors.md) and [feno.yaml](feno-yaml.md). See the
    [README](https://github.com/dennisme/feno#readme) for what is being built
    and in what order, and
    [spec.md](https://github.com/dennisme/feno/blob/main/spec.md) for why.

## The idea in one page

Anomaly detection on metrics is well understood and barely adopted, because
the algorithms were never the hard part. Tuning them is. A band that pages
forty times a month gets muted in a quarter, and nothing in the open source
tool chain tells you that will happen before you merge it.

So feno is three commands over one specification.

`suggest` writes the first draft of that specification from an SLI the
repository already holds. It profiles the series over three weeks and decides
the fields nobody can fill in from the document alone, seasonality from the
autocorrelation at a day and at a week, the strategy from how spiky the series
is, and it puts the measurement behind every one of them in the draft as a
comment. The fields it cannot measure are left out so the defaults apply, and
a series too sparse or too spiky to band is refused with the number that says
so rather than drafted anyway. Nothing in it calls out to a model.

`generate` compiles a detector into recording and alerting rules for the
engine you already run, which means Prometheus and its compatible stores
first, and ClickHouse through
[clickhouse-ruler](https://github.com/dennisme/clickhouse-ruler) after that.
There is no new service to operate and no anomaly score to learn to trust:
the output is rules a person can read at three in the morning.

`backtest` replays a detector over the last month of real history and reports
what it would have done. How many separate times it would have paged, for how
long, which series were responsible, and how much of it coincided with an
error budget actually burning. That report is the whole point. A detector that
never lined up with a real incident is a detector to delete, and the report
says that in those terms: how many of the paging episodes coincided with the
linked SLO's budget being spent, and how much of the paged wall clock did. A
detector that names no SLO says so rather than printing a zero, because an
overlap of zero over a budget that was read is the finding and the two must
not read alike.

A detector over its noise budget fails the build, and a detector that has to
stay over it for now carries a `waiver` in `feno.yaml`: a reason, and a date
it runs out on. Until that date the finding is still reported, named as
waived, and the build passes. After it the waiver is itself a finding and the
build fails, so the decision comes back to a reviewer instead of living on as
a raised number nobody remembers writing.

## Where to look things up

| Page | What it answers |
| --- | --- |
| [Installing feno](install.md) | A release archive with its checksum, `go install`, and what `feno version` reports |
| [Writing a detector](detectors.md) | Every field of a detector document: what `strategy`, `sensitivity`, `window`, `minCoverage` and the rest mean, their defaults, and what leaving one out does |
| [feno.yaml](feno-yaml.md) | Targets, the noise budget, and waivers: how the file is found and which limit binds when two files set one |
| [Reading a backtest report](report.md) | The report line by line: episodes against flaps, the two windows, instants with no verdict, and the budget overlap |
| [Running the gate in CI](ci.md) | Exit codes, which checks need a server, and the workflow |

## Why it is shaped this way

The specification is [OpenSLO](https://openslo.com/)'s, extended rather than
replaced. A team that already declared a `DataSource` and an `SLI` has
already written down where the metrics live and which query matters, so feno
asks for neither again.

The long version, including the landscape it is answering and the decisions
already settled, is in
[spec.md](https://github.com/dennisme/feno/blob/main/spec.md).
