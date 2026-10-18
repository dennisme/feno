# Reading a backtest report

`feno backtest` replays a detector over history the datasource already holds
and prints what it would have done. Nothing is deployed and nothing is
written: it needs read access and that is all.

This page walks the report line by line.

## A whole report

Captured from a real run against a public Prometheus this repository does not
operate:

```console
$ feno backtest -f examples/over-budget --url https://demo.promlabs.com --from -14d
demo-api-latency-aggressive  robust, iqr, aggressive, window 15m, 12 samples minimum
  query                 histogram_quantile(0.90, sum by (le) (rate(demo_api_request_duration_seconds_bucket[5m])))
  step                  1m, for 2m
  requires              nothing optional
  reported window       2026-10-02T06:06:00Z .. 2026-10-16T06:06:00Z  (336h, 20161 instants)
  history read          2026-10-02T05:51:00Z .. 2026-10-16T06:06:00Z  (336h15m, warm-up 15m)
  read cost             2 requests, 20176 points, 1 series

  paging episodes       9, plus 908 flaps that crossed the band without reaching for
  series firing time    9m  (0.04% of the reported window, summed over series)
  paged wall clock      9m  (0.04% of the reported window)
  peak concurrent       1 series firing at 2026-10-03T23:38:00Z
  longest episode       1m from 2026-10-03T23:38:00Z  {}
  without a verdict     0 of 20161 series-instants: 0 cold, 0 absent, 0 undefined
  budget overlap        no linked SLO: set spec.sloRef to the SLO this detector
                        protects, or delete a detector nobody can tie to a burn

  series  firing  flaps  firing time  ratio  cold  absent  undefined
  {}      9       908    9m           0.04%  0     0       0

  Every ratio above divides by the reported window. Dividing by the span
  the samples cover, or by the window times the series count, reads lower
  and flatters the detector: the first hides a series that was missing,
  the second shrinks every time the query matches another instance.
```

Exit code 1, and on stderr:

```text
demo-api-latency-aggressive: 9 paging episodes, limit 0 (examples/over-budget/feno.yaml)
```

`examples/over-budget` is a tree built to be red: the same latency quantile
the green example bands, read at a quarter of the window on the
[aggressive sensitivity](detectors.md#sensitivity), under a
[`maxEpisodes: 0`](feno-yaml.md#budget) nothing can satisfy. Nine pages in two
weeks on flat demo traffic is what an over-tuned detector looks like.

## The header

The first line is the band, in the terms the document wrote it in, so a
report pasted into a ticket says which band produced it. A seasonal band also
says how many offsets it read, and the sample count it prints says `per
window`, because that is the window the floor applies to.

| Line | What it says |
| --- | --- |
| `query` | The query that was read, after any `metricSourceRef` resolved |
| `step` | The query resolution, and the detector's `for`. A backtest at a 1m step predicts a rule evaluated at 1m and no other |
| `requires` | Optional server features this band needs, or `nothing optional`. A `robustScale: mad` band says `median absolute deviation` here |
| `reported window` | The range you asked for, which every ratio below divides by |
| `history read` | The longer range that had to be read, and the warm-up that is the difference |
| `read cost` | Requests, samples and series. A long window at a fine step is many requests, stitched together |
| `server warning` | Printed once per warning the server returned, when it returned any |

`step` is one value doing four jobs: the query resolution, the grid instants
are matched on, the gap that tells an absence from a consecutive sample, and
what turns [`minCoverage`](detectors.md#mincoverage) into a sample count.

### Why two windows

A band needs history *before* an instant to be computable at it. So a request
for a verdict on fourteen days is a read of fourteen days plus the band's
lookback: the window above, plus 15m for this flat band, and three weeks plus
the window for a weekly one.

The boundary goes that way round on purpose. Reading fourteen days and
reporting on whatever survived the warm-up would answer a question nobody
asked, and answer it differently for every detector in the same directory.
Both spans are printed because firing time as a fraction means nothing
without knowing which window it is a fraction of.

## Episodes and flaps

An episode is one run of breaching instants on one series. It is a **paging
episode** when it lasted long enough to survive the detector's
[`for`](detectors.md#alerting), and a **flap** when it crossed the band and
recovered first. A flap woke nobody. Many flaps and few episodes means `for`
is already doing its job; many episodes is the finding.

908 flaps against 9 episodes on the report above is the shape of a band that
is too tight, held back by a 2m `for`.

The unit counted is the series episode, because that is the unit Prometheus
pages on: an alerting expression produces one alert per label set, so four
instances breaching the same minute is four pages and not one incident. The
headline is the sum across series, and the table at the bottom says which
label sets produced them, worst first.

| Line | What it says |
| --- | --- |
| `paging episodes` | Episodes that survived `for`, summed over series, and the flaps that did not |
| `series firing time` | Firing time summed over series. Three instances firing all month is three hundred percent of a month of pages, and that is the honest reading |
| `paged wall clock` | Time during which at least one series was firing. This is the one a person experienced |
| `peak concurrent` | The most series firing at once, and when |
| `longest episode` | The longest single page, and the series it was on |

The last three are what separate a detector that pages nine times across two
weeks from one that pages nine times in the same ten minutes.

## Instants with no verdict

```text
  without a verdict     0 of 20161 series-instants: 0 cold, 0 absent, 0 undefined
```

An instant with no verdict cannot breach, so a detector with nothing firing
and a third of its window cold has *not* been shown to be quiet. The count is
of series-instants, because every series is evaluated at every instant.

| Status | What happened |
| --- | --- |
| `cold` | Too few samples in a window to compute a band. A seasonal band is cold when any one of its offset windows is short, however full the others are. Raise or lower [`minCoverage`](detectors.md#mincoverage) once you know which |
| `absent` | The series had no sample at that instant |
| `undefined` | A band was computed and the comparison could not be made, such as a NaN value |

A query that returned nothing at all over the window read prints no numbers
and says so instead: that is the absence of data rather than a quiet
detector, and the two must not read alike.

## Budget overlap

The most useful thing the report says. An anomaly episode that never
coincided with an error budget actually burning is a candidate for deletion.

It is an intersection of two timelines, so it needs a second read. The SLO
named by [`sloRef`](detectors.md#sloref) is compiled into a burn query, read
over the reported window at the same step, and intersected with the firing
episodes. Nothing has to be deployed, and the burn signal is compiled from
the SLO rather than looked for in sloth's or pyrra's recording rules, whose
names feno would have to guess.

Each outcome is a different sentence rather than a number, because the zeroes
mean different things and telling them apart is the whole point:

| What the line says | What it means |
| --- | --- |
| `no linked SLO: set spec.sloRef to the SLO this detector protects, or delete a detector nobody can tie to a burn` | The detector names no SLO, as in the report above. Not an overlap of zero |
| `not computed: the burn query answered at no instant of the window, which is the absence of a budget rather than a quiet one` | There is a link, and the burn query returned nothing |
| `nothing fired, so nothing coincided with the budget burning` | A quiet detector. There was nothing to intersect |
| `3 of 9 paging episodes coincided with the budget burning` | The real answer. A second line gives the coincident wall clock out of the paged wall clock, as a percentage |

An overlap of zero over a budget that *was* read is the finding: a detector
whose pages never landed on anything real.

A linked report carries three more lines, and a `coincident` column in the
table:

| Line | What it says |
| --- | --- |
| `linked SLO` | The SLO and its objective |
| `budget burning` | How many instants the budget was burning at, and how many had no answer |
| `burn signal` | The query that was read, and the comparison applied to it |

The coincident percentage is the one ratio in the report that does not divide
by the reported window. It divides by the time somebody was paged, because
the question behind it is what fraction of the pages landed on a budget that
was being spent.

## The series table

One row per label set the query returned, worst first. Same columns as the
summary, per series, plus `coincident` when there is an SLO to be coincident
with. There is no column of zeroes when there is no link: a column of zeroes
would read as a verdict.

The per-series ratios each divide by the whole window, so they can sum past
100%. That is deliberate, and the closing paragraph of every report says
which denominator was used and which two alternatives would have flattered
the detector.

## What the exit code does with all this

The report is printed either way. Whether the build stops is the
[budget](feno-yaml.md#budget)'s decision, and the codes are in
[Running the gate in CI](ci.md#what-each-exit-code-means).
