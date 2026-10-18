# Writing a detector

A detector is one YAML document in the same directory as the
[OpenSLO](https://openslo.com/) documents it sits beside. This page is the
field list: what each field means, what it does when you leave it out, and
what the empty value says.

Why the specification is OpenSLO's at all, and what the landscape looks like,
is in
[spec.md](https://github.com/dennisme/feno/blob/main/spec.md). The
specification itself is
[OpenSLO/OpenSLO](https://github.com/OpenSLO/OpenSLO), and the types feno
imports are
[go-sdk's v1 package](https://pkg.go.dev/github.com/OpenSLO/go-sdk/pkg/openslo/v1).

## A whole document

This is `examples/demo/detectors/demo-api-latency.yaml`, which the gate in
this repository runs against a public Prometheus every day.

```yaml
apiVersion: anomaly.openslo.io/v1alpha
kind: AnomalyDetector
metadata:
  name: demo-api-latency
spec:
  indicator:
    metadata:
      name: demo-api-latency
    spec:
      thresholdMetric:
        metricSource:
          type: Prometheus
          spec:
            query: histogram_quantile(0.90, sum by (le) (rate(demo_api_request_duration_seconds_bucket[5m])))
  detection:
    strategy: robust
    robustScale: iqr
    class: latency
    sensitivity: balanced
    window: 2h
  alerting:
    for: 10m
```

`feno suggest --from-sli slis/ --url <endpoint> -o detectors/` writes a first
draft of this from an SLI the repository already holds, with the measurement
behind each field it chose as a comment. The fields it could not measure are
left out, so the defaults below apply.

## The document itself

| Field | What it is |
| --- | --- |
| `apiVersion` | Always `anomaly.openslo.io/v1alpha`. A separate API group from OpenSLO's, so [`oslo validate`](https://github.com/OpenSLO/oslo) can tell which documents in the directory are not its business. Any other value is refused |
| `kind` | Always `AnomalyDetector`, the only kind this group defines |
| `metadata` | OpenSLO's [`Metadata`](https://pkg.go.dev/github.com/OpenSLO/go-sdk/pkg/openslo/v1#Metadata). `name` is required: it is what a generated rule group is called and what every error points at |
| `spec` | Everything below |

## What the detector watches

| Field | Default | What it is |
| --- | --- | --- |
| `description` | empty | Free text for a reader. Nothing reads it |
| `indicator` | required | An inline OpenSLO indicator. Exactly one `thresholdMetric`, because a band is over one series |
| `sloRef` | empty | The name of the SLO whose error budget this detector is judged against |

`indicator` is OpenSLO's
[`SLOIndicatorInline`](https://pkg.go.dev/github.com/OpenSLO/go-sdk/pkg/openslo/v1#SLOIndicatorInline),
reused rather than re-specified. The query goes under `thresholdMetric.metricSource`, and that
metric source sets exactly one of two things:

- `type`, naming the datasource kind inline, as the example above does.
- `metricSourceRef`, naming a `DataSource` document in the same tree. The
  reference carries the connection and never the query, so `spec.query` is
  required either way.

A `ratioMetric` is refused rather than supported. A ratio is two or three
queries and a band reads one series, so composing good and total into one
series is a judgement about what the ratio means that a loader has no
business making quietly. When `feno suggest` drafts from a ratio SLI it takes
the ratio's `total`, because the burn alert already watches the ratio against
the objective, and what no ratio can see is its own denominator collapsing.

### sloRef

`sloRef` is what makes the budget overlap in a
[backtest report](report.md#budget-overlap) computable: an episode that never
coincided with that SLO's budget burning is a candidate for deletion.

It is a field of its own rather than something inferred. A shared
`metricSourceRef` is the connection and many SLOs share one, so it says
nothing about which objective a detector protects, and an indicator name that
happens to match an SLI is a link that disappears the day somebody renames
one.

Empty is allowed, and is a detector nobody has tied to an objective. The
report says that in words rather than printing a zero, because an overlap of
zero over a budget that *was* read is itself the finding, and the two must not
read alike. A `sloRef` naming a document that does not exist, or one that is
an SLI rather than an SLO, is an error: a typo must not pass as "no link".

## `detection`

The band, in the terms you write rather than the terms it is computed in.

| Field | Default | What it is |
| --- | --- | --- |
| `strategy` | required | `adaptive` or `robust`: how the centre and the spread are taken from the window |
| `robustScale` | `iqr` when `strategy: robust` | `iqr` or `mad`: which measure of spread `robust` uses. Must be empty on `adaptive` |
| `sensitivity` | required | `aggressive`, `balanced` or `conservative`: how far the band reaches either side of its centre |
| `seasonality` | empty, a flat window | `daily`, `weekly`, or both: the periods history is read from. Not available on `strategy: robust` |
| `class` | required | `requests`, `latency`, `errors` or `saturation`: what kind of signal this is |
| `window` | `1h` | How much history one band is computed over |
| `minWidth` | `0`, no floor | A floor on the band's full width, in the series' own units |
| `minCoverage` | `0.75` | The fraction of a full window that must hold samples before a band is computed at all, required of every window a seasonal band reads |

### strategy and robustScale

`adaptive` is the mean plus the standard deviation. It fits a series that
sits roughly normally around its own level, and it is the wrong choice for a
spiky one, where a single spike widens the band enough to hide the next one.

`robust` is the median plus a robust scale, which a spike moves far less. The
two scales are interchangeable by design and both are converted to the same
units, so a band does not change width when you switch:

- `iqr`, the interquartile range, reads two order statistics and needs no
  optional server feature. That is what makes it usable as the default.
- `mad`, the median absolute deviation, reads every sample's distance from the
  median. On Prometheus it needs `--enable-feature=promql-experimental-functions`,
  so a band using it declares that requirement and the
  [report](report.md#the-header) prints it.

Leaving `robustScale` out under `strategy: robust` is the usual case: `iqr`
is filled in, because the reason to prefer it is a property of the server the
rule will run on rather than of the band. Write `robustScale: mad` when you
have that feature on.

`robust` and `seasonality` together are refused, by `feno generate` and by
`feno backtest` alike. A seasonal band reads one window per offset and
combines them by their means and their spreads, and no combination of
per-window medians is the median of the windows together, so there is no such
band to compute. Pick one: the seasonal shape, with `adaptive`, or the robust
scale, over a single window.

### sensitivity

Sensitivity is the knob to turn first. It sets how many units of scale the
band extends either side of its centre.

| Value | Scale units | Outside the band, on normal data |
| --- | --- | --- |
| `aggressive` | 2 | about 4.6% of samples |
| `balanced` | 3 | about 0.27% |
| `conservative` | 4 | about 0.006% |

`conservative` means conservative about paging somebody, so it is the widest
band and the fewest alerts.

### seasonality

Empty is a flat window: one band from the `window` immediately before the
instant. That is right for a series with no daily shape, and wrong for one
where 3am and 3pm are different services.

`daily` reads the same time of day on the preceding days. `weekly` reads the
same time of week on preceding weeks, which is the one that tells a Tuesday
from a Sunday. Both together read both.

Four things follow from naming any:

- Three periods back are read, not two. Two windows give a centre that is
  just their midpoint, which is a weaker description of a normal day than
  three.
- Every window weighs the same, whatever it holds. The centre is the average
  of the per-window averages, and the spread is taken from the spread inside
  the windows and the spread between them. A window thinned by a scrape gap
  therefore counts as much as a full one, which is what keeps a backtest and
  the generated rule on the same number: a rule reads each offset with its
  own range selector and cannot weigh one against another.
- `window` becomes the width of the window taken *at each offset*, not a span
  reaching back to them.
- A backtest has to read further back before it can report anything: three
  weeks plus the window, for a weekly band. The report prints both spans.

Order does not matter. `[weekly, daily]` and `[daily, weekly]` produce the
same band and the same rule file, so a diff that only reorders the list
changes nothing. Naming the same period twice is an error.

### class

`class` says what kind of signal the series carries: `requests`, `latency`,
`errors` or `saturation`.

Nothing in the band reads it today. It is required and carried because it is
the field that will decide whether a breach *below* the band counts: a drop in
request rate is an incident, and a drop in error rate is a Tuesday. One-sided
detection is not implemented, and approximating it from the class would be a
guess.

### window

`1h` when the document does not say. An hour is the shortest window that
holds enough samples to estimate a spread at every step a real Prometheus
runs: 240 samples at a 15s scrape, 60 at a 1m evaluation interval, both far
above the two a standard deviation needs. It is also short enough that a
shift in level over an hour is still outside the band rather than absorbed
into its centre, which is the failure mode of a long window and the reason a
day is not the default.

`window: 0h` is an error rather than "absent". The unit is what says you
wrote something.

A window that holds fewer than two samples at the step it will be evaluated
at is refused, with the count, because a spread needs two.

### minWidth

A floor on the band's full width, in whatever units the series is in. It is
what stops a near-flat series from paging on noise: when the spread over the
window is almost zero, the band collapses onto the centre and any wobble is
outside it.

`0` is the default and means no floor. There is no value derived from
`class`, because the units are the series' own and nothing in feno knows
them: a floor of `1` is sensible for a count of errors and meaningless for a
ratio.

### minCoverage

The fraction of a full window that must hold samples before a band is
computed at that instant. An instant with too few samples gets no verdict, is
counted as `cold` in the [report](report.md#instants-with-no-verdict), and
cannot breach.

`0.75` when the document does not say, and it is a fraction rather than a
sample count on purpose: the count depends on the step, the step differs
between a backtest and a ruler for the same detector, and you cannot be asked
to track it. feno turns the fraction into a count from whatever step is about
to be used, with a floor of two samples.

A band with `seasonality` set asks it of every window separately, not of all
the samples together. One empty offset window gets no verdict even when the
other two hold a full window each, which is what the generated rule does: it
reads each offset with its own range selector, and one over an empty range
returns nothing, so the whole chain drops the series. A fraction of all the
samples together would let feno report breaches in a backtest that the rule
on the server could never produce.

Three quarters is a judgement, so it is worth saying which way each direction
is wrong. Too high and an ordinary scrape gap or a rolling restart turns
instants cold, so a backtest reports warm-up where the series was merely thin.
Too low and a band computed from a handful of samples either side of an outage
is a confident number taken from almost nothing, reported as not breaching.
The second is the one that makes a backtest lie, so the default sits nearer
the strict end. Lower it when you know the series is sparse.

Anything outside `(0, 1]` is refused.

## `alerting`

| Field | Default | What it is |
| --- | --- | --- |
| `for` | empty | How long a breach has to hold before anybody is paged. It is the emitted rule's `for`, and the line a backtest draws between an episode and a flap |
| `runbook` | empty | A URL, emitted as an annotation on the generated rule |

`for` is the second knob after `sensitivity`, and the cheaper one. A breach
that crosses the band and recovers before `for` elapses is a flap: the
[report](report.md#episodes-and-flaps) counts it separately and nobody was
woken for it. A detector with many flaps and few episodes is one `for` is
already doing its job on.

Empty `for` means every breach is an episode from its first instant.

`runbook` is carried so that one document describes the whole detector.
Nothing in feno reads it; it ends up in the rule file.
