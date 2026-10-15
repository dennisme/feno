# Running the gate in CI

feno is a gate somebody else operates. Everything a workflow needs from it is
in the exit code, so that is where this page starts.

## What each exit code means

| Code | What happened | What a workflow should do |
| --- | --- | --- |
| 0 | Every detector is within its noise budget. A waived finding is still printed, and still exits 0 | Merge |
| 1 | A detector is over its budget, or a waiver has expired, or nothing was worth drafting | Fail the build |
| 2 | feno could not run: a flag, a path, a malformed `feno.yaml`, a query the server rejected | Fail the build. This one is never the detector's fault |
| 3 | The datasource was never reached, so the budget went unchecked | Decide. See below |

Exit 3 is the one that needs a decision. A runner with no network path to
production has not found a quiet detector and has not found a noisy one, and
reusing either of the other codes would say something untrue. The two offline
checks still ran, so what is unchecked is the noise budget alone.

A repository reading its own datasource should probably fail on 3: a
datasource it operates being unreachable is its problem. A repository reading
somebody else's should warn, which is what the example below does.

## What is offline and what is not

| Check | Needs a server |
| --- | --- |
| `feno generate --check` | No |
| `promtool check rules` | No |
| `feno backtest` | Yes, read access |
| `feno generate --url` | Yes, one instant query |

The two offline checks work on a fork pull request and on a runner with no
path to production, and nothing in `feno.yaml` waives them. They are
correctness: a rule that fails either cannot do its job.

## The workflow

This is [`.github/workflows/gate.yaml`](https://github.com/dennisme/feno/blob/main/.github/workflows/gate.yaml),
which this repository runs against a public Prometheus it does not operate.

```yaml
name: Gate

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: Build feno
        run: go build -o feno ./cmd/feno

      - name: Drift gate
        run: ./feno generate -f detectors/ --check

      - name: Check the committed rules
        run: promtool check rules rules/anomaly/*.yaml

      - name: Noise budget
        run: |
          set +e
          ./feno backtest -f detectors/ --url "$PROMETHEUS_URL" --from -14d
          code=$?
          set -e
          case $code in
            0) echo "::notice::every detector is within its budget" ;;
            3) echo "::warning::the datasource was not reachable, so the noise budget went unchecked" ;;
            *) exit $code ;;
          esac
        env:
          PROMETHEUS_URL: ${{ vars.PROMETHEUS_URL }}
```

A multi-tenant datasource takes its tenancy on the command line rather than in
the URL:

```bash
feno backtest -f detectors/ --url "$PROMETHEUS_URL" --from -14d \
  --header "X-Scope-OrgID=$TENANT" --header "Authorization=Bearer $TOKEN"
```

## A waived run

A detector that has to stay over budget for now carries a waiver with a reason
and an expiry, and the run exits 0 with the finding still printed:

```console
$ feno backtest -f detectors/ --url "$PROMETHEUS_URL" --from -14d
waived: checkout-request-rate: 10 paging episodes, limit 4 (detectors/feno.yaml)
the waiver in detectors/feno.yaml runs out on 2026-12-05T00:00:00Z: black friday traffic shape, retuning after the freeze
```

Nothing special is needed in the workflow: a waived run is a green one. Past
the date the same run exits 1 and names the waiver that expired, whether or
not the detector is still over budget, so the line is deleted or renewed in
front of a reviewer rather than out of habit.

## A server that does not answer

Every request carries a deadline, `--request-timeout`, defaulting to 30
seconds. It is per request and not per run: a run that gave up halfway would
have reported some detectors and not others, and exit 3 would then be a claim
about the whole run that is not true of it. A deadline that fires is the same
finding as a refused connection, so it exits 3 and the degrade above covers
it.

Raise it for a datasource that is slow rather than absent:

```bash
feno backtest -f detectors/ --url "$PROMETHEUS_URL" --from -30d --request-timeout 2m
```

`--request-timeout 0` waits forever, which is for a laptop asking for a
quarter of history on purpose and never for CI.

Interrupting a run with Ctrl-C, or a runner sending SIGTERM at the end of a
job, cancels the read in flight and keeps the reports already printed. The run
exits 3: part of the budget was checked and the rest was not.

## Where the files go

`-f` names a tree of specifications. Generated rules are committed, but not
inside that tree: the loader reads every YAML file under `-f` and a generated
rule file is not a specification.

```text
alerting/
  feno.yaml                   # the budget, and the target the rules go to
  detectors/
    checkout-request-rate.yaml
rules/anomaly/
  checkout-request-rate.yaml  # generated, committed, checked by promtool
```

[`examples/demo`](https://github.com/dennisme/feno/tree/main/examples/demo) is
that layout, with the rules in `examples/rules`, and it is what the workflow
above runs.

## Proving the codes rather than trusting this page

`just demo` runs the whole table against the public demo server: the drift
gate, a tree within budget, a tree over it, a waiver on either side of its
expiry, and a port nothing is listening on. It is a job in the same workflow,
so a code that changed meaning fails here rather than in somebody's
repository.
