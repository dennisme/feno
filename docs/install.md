# Installing feno

feno is one static binary. It reads specifications, talks to a datasource over
HTTP, and writes rule files. There is no service to run and no state to keep,
so installing it is putting one file on a path.

## A release archive

Releases are at
[github.com/dennisme/feno/releases](https://github.com/dennisme/feno/releases).
Each one carries archives for Linux and macOS on `x86_64` and `arm64`, and a
`checksums.txt` covering all of them.

```bash
version=0.1.0
os=$(uname -s)                       # Linux or Darwin
arch=$(uname -m)                     # x86_64 or arm64
base=https://github.com/dennisme/feno/releases/download/v$version

curl -sSLO "$base/feno_${version}_${os}_${arch}.tar.gz"
curl -sSLO "$base/checksums.txt"
shasum -a 256 -c checksums.txt --ignore-missing

tar -xzf "feno_${version}_${os}_${arch}.tar.gz" feno
install -m 0755 feno /usr/local/bin/feno
```

The checksum step is the one worth not skipping. `--ignore-missing` is what
lets one `checksums.txt` be checked against the single archive you downloaded
rather than all four.

## go install

```bash
go install github.com/dennisme/feno/cmd/feno@latest
```

This builds from source with your own toolchain, so it needs Go and it does
not stamp a version: `feno version` reports `dev`, with the commit and the
build time from the toolchain's own record. Pin a release instead of `@latest`
when a workflow has to be reproducible:

```bash
go install github.com/dennisme/feno/cmd/feno@v0.1.0
```

## In a workflow

A runner wants the archive rather than a build. The gate this repository runs
against a public Prometheus does exactly that, and the whole file is
[`.github/workflows/gate.yaml`](https://github.com/dennisme/feno/blob/main/.github/workflows/gate.yaml).
See [Running the gate in CI](ci.md) for what each exit code means and which
checks need a server at all.

## From source

```bash
git clone https://github.com/dennisme/feno
cd feno
just build
```

`just init` installs the pinned tool versions through
[mise](https://mise.jdx.dev/) and the pre-commit hooks, which is what a
contributor wants. `just check` is everything CI runs.

## Confirming what you installed

```console
$ feno version
feno 0.1.0
commit 9116755b1992a04040cc08c6b2cc43af87addc9d
built  2026-10-16T16:44:13Z
dirty  false
go     go1.26.9
```

`version` is the one command that reads no datasource and no specification.

That is the v0.1.0 archive. A binary built from a working tree prints `dev`
in place of the release, and `dirty true` says that tree had uncommitted
changes in it. The commit, the build time and the dirty flag come from the
toolchain's own record rather than from anything stamped at release time, so
they are there either way.

## What to read next

| Page | What it answers |
| --- | --- |
| [Writing a detector](detectors.md) | Every field of a detector document |
| [feno.yaml](feno-yaml.md) | Targets, the noise budget, and waivers |
| [Reading a backtest report](report.md) | The report line by line |
| [Running the gate in CI](ci.md) | Exit codes and the workflow to copy |
