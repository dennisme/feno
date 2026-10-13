# feno

Anomaly detector specifications, written as OpenSLO-shaped YAML in git,
compiled into the alert rules your stack already evaluates, and replayed over
history before they ship.

!!! warning "Status: both commands run against Prometheus"

    `feno backtest -f detectors/ --url <endpoint> --from -30d` replays a
    detector over history and prints the report, and `feno generate -f
    detectors/ -o rules/anomaly/` compiles the rule files, with `--check` as
    the drift gate. The ClickHouse target is next. See the
    [README](https://github.com/dennisme/feno#readme) for what is being built
    and in what order, and
    [spec.md](https://github.com/dennisme/feno/blob/main/spec.md) for why.

## The idea in one page

Anomaly detection on metrics is well understood and barely adopted, because
the algorithms were never the hard part. Tuning them is. A band that pages
forty times a month gets muted in a quarter, and nothing in the open source
tool chain tells you that will happen before you merge it.

So feno is two commands over one specification.

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
never lined up with a real incident is a detector to delete. The budget
overlap is the one number not computed yet, and the report says so rather than
printing a zero.

## Why it is shaped this way

The specification is OpenSLO's, extended rather than replaced. A team that
already declared a `DataSource` and an `SLI` has already written down where
the metrics live and which query matters, so feno asks for neither again.

The long version, including the landscape it is answering and the decisions
already settled, is in
[spec.md](https://github.com/dennisme/feno/blob/main/spec.md).
