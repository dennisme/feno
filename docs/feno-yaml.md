# feno.yaml

`feno.yaml` sits beside the detectors it governs and carries the two things
the repository decided once: where generated rules go, and what counts as too
noisy.

A URL, a header and a step are not in it. A URL is where one runner can reach
one server, a header carries a tenant or a credential, and neither belongs in
a committed file. Those stay flags.

The name is not configurable. The point of the file is that a reviewer knows
where to look for the budget without being told.

## A whole file

This is `examples/demo/feno.yaml`.

```yaml
targets:
  - engine: prometheus
    output: ../rules

budget:
  maxEpisodes: 4
  maxFiringRatio: 0.02
```

Both top-level fields are optional. A file with no `budget` holds no detector
to anything, and a repository with no `feno.yaml` at all is not an error:
`feno backtest` reports and exits 0.

| Field | What it is |
| --- | --- |
| `targets` | Where the rules for each engine are written |
| `budget` | The noise budget: the limits a backtest holds a detector to |

## How the file is found

`feno generate` and `feno backtest` walk up from the specifications they were
pointed at, so you normally pass no path. `-config path/to/feno.yaml` names
one instead.

Every file from the path the command was pointed at down to a detector's own
directory applies, resolved per detector. That is what lets a monorepo keep a
ceiling at the root and a tighter budget per team in the same run.

Where more than one file sets the same limit, **both apply and the stricter
binds**. A team can tighten what the repository set and cannot loosen it,
which is the same argument that put the budget in a file rather than in a
workflow. A team entry looser than the repository's is dead config rather
than an error, because the two files have different authors and a
repository-wide tightening should not break every team's build at once.
Inside one file it *is* an error: one author owns that file and can fix it.

The walk stops at the path the command was pointed at. A file above that was
never asked for, and a budget that binds because of where somebody was
standing is the thing this avoids. Point at the tree to ask for the
repository-wide ceiling.

A misspelled field fails the load. A misspelled `maxEpisodes` would otherwise
be a budget that was written down and never enforced.

## targets

| Field | What it is |
| --- | --- |
| `engine` | Which engine the rules are for. `prometheus` today |
| `output` | The directory the rule files are written to, relative to this file |

Both are required. Two targets for one engine is an error, because taking
either would make the output directory depend on the order the file happened
to list them in.

The engine name is not checked against a list here. A file naming an engine
this build has no target for is refused by the command you asked for it, with
the list of what it does have.

## budget

| Field | Default | What it is |
| --- | --- | --- |
| `maxEpisodes` | unset, no limit | How many times a detector may have paged over the backtest's window |
| `maxFiringRatio` | unset, no limit | The fraction of that window a detector may have been paging for, between 0 and 1 |
| `detectors` | empty | Per-detector limits, keyed by `metadata.name` |

`maxEpisodes` and `maxFiringRatio` at the top of `budget` are this file's
defaults, applying to every detector it governs. Both are read from the
[report](report.md): episodes are the headline count, summed over series, and
the ratio is paged wall clock over the reported window.

**Unset is not zero.** A missing limit holds a detector to nothing. `0` is a
limit somebody meant, for a detector that may never page at all.

A negative `maxEpisodes`, or a `maxFiringRatio` outside 0 to 1, is refused
with the field path it was written at.

Being over either limit is exit 1 and a line on stderr naming the number, the
limit, and the file the limit came from:

```text
demo-api-latency-aggressive: 9 paging episodes, limit 0 (examples/over-budget/feno.yaml)
```

The file is in that message because "over budget" is not actionable until a
reader knows which file set the budget. There is no line number: the YAML is
decoded through JSON, which does not carry one, and a second parser to report
it is more than the message is worth.

### detectors

`detectors` holds limits for the ones that need their own, keyed by the
detector's `metadata.name`:

```yaml
budget:
  maxEpisodes: 4
  maxFiringRatio: 0.02
  detectors:
    checkout-request-rate:
      maxEpisodes: 2
```

A field the entry does not name keeps this file's default, so the detector
above is held to 2 episodes and still to a ratio of 0.02. A detector the file
does not name gets the defaults. A name that no loaded detector declares is
not an error: the specifications one backtest was pointed at are often a
subset of what the repository holds.

Inside one file, an entry looser than the file's own default is refused.

## waiver

A detector over its budget has two ways through a pull request: tune it, or
raise the number. The second is how a budget rots, because the raise outlives
the incident that justified it.

A `waiver` is the third way. It keeps the finding, lets the build pass, and
starts failing the build once its date passes, so the decision comes back in
front of a reviewer instead of living on as a raised number nobody remembers
writing.

```yaml
budget:
  maxEpisodes: 0
  detectors:
    demo-api-latency-aggressive:
      waiver:
        reason: the demo server is somebody else's, and this tree exists to show a red gate
        until: 2026-12-05
```

| Field | What it is |
| --- | --- |
| `reason` | Why the detector is over budget. Required |
| `until` | When it stops applying. Required |

Both are required, and for the same reason: a waiver nobody explained cannot
be reviewed when it comes up for renewal, and a waiver with no expiry is the
raised number with extra words on it.

A waiver goes on a detector entry only. One written over a file's defaults
would waive every detector that file governs, including the ones nobody
looked at, while the reason it states is about one of them, so it is refused.

### Before and after the date

| | Exit code | What is printed |
| --- | --- | --- |
| Within the waiver | 0 | The finding, marked `waived:`, and the date it runs out on |
| Past `until` | 1 | The expired waiver as a finding of its own, plus any finding it used to cover |

The expiry is a finding whether or not the detector is still over budget. A
waiver whose date has passed on a detector somebody already tuned is a line
that means nothing, and the build failing is what gets it deleted rather than
renewed out of habit.

### How until is read

The instant itself is already expired, so `until: 2026-12-05` covers December
the fourth and not the fifth.

A bare date is midnight UTC, whatever the machine running the backtest is set
to. A waiver that expired at a different moment in CI than on a laptop would
be a finding that depends on where it ran. Write the RFC3339 form with its
offset when you need a specific local moment:

```yaml
until: 2026-12-05T18:00:00-05:00
```

Reports print the date back in RFC3339, because printing a bare date back
would hide which midnight it was.

### What a waiver does not cover

Nothing in this file waives the offline checks. `feno generate --check` and
`promtool check rules` are correctness, not noise: a rule that fails either
cannot do its job. See [Running the gate in CI](ci.md#what-is-offline-and-what-is-not).

A nested file's waiver replaces the one above it rather than merging with it.
"Stricter" does not order a waiver the way it orders two numbers.
