# Contributing

## Before you start

[spec.md](spec.md) is the source of truth for intended behaviour. If the code
and the spec disagree, say so rather than picking one silently. A change that
contradicts a decision already recorded there wants that file updated in the
same pull request, in its own commit ahead of the code.

The project is early and has no external users, so names, flags and output can
change outright. Nothing here needs a migration path or a compatibility shim.

## Setup

```bash
just init     # mise tool versions and pre-commit hooks
just check    # lint, build, unit tests, promtool, markdownlint
```

`just check` is what CI runs, in the order CI runs it. Run it before claiming
work is done. If you touched Markdown, that includes `just markdownlint`,
which CI runs too.

## Commits

Conventional Commits, enforced by commitizen (`.cz.toml`) through a
`commit-msg` hook and a CI check. Allowed types: `feat`, `fix`, `doc`, `perf`,
`ref`, `test`, `chore`, `ci`, `revert`.

```text
<type>(<optional scope>): <subject>
```

One line. Subjects end up in the release notes, which GitHub generates per
pull request, so reasoning belongs in the pull request body rather than in a
commit body.

Never bypass a pre-commit hook.

## Tests

- Table-driven, with fixtures in each package's `testdata/` rather than large
  YAML strings embedded in the test.
- Test output must be clean. A test that expects an error captures and asserts
  the error output instead of letting it print.
- No mocked data sources. Anything that needs a real Prometheus or a real
  ClickHouse is an integration test against a real one, behind a build tag.

## Comments

Comments explain why a thing is the way it is, and the `justfile` does the
same. Match that. Never write a comment about what the code used to do or
that something changed.
