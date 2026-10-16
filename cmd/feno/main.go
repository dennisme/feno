// Command feno compiles anomaly detector specifications into alert rules and
// replays them over history.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dennisme/feno/internal/buildinfo"
)

// Exit codes. A finding, a failure to run and a check that could not run are
// separated so CI can tell "your specs are wrong" from "the tool could not
// run" from "nobody checked".
//
// exitSkipped exists because of how the pull request gate degrades. A runner
// with no network path to production cannot backtest, and the two codes it
// would otherwise reuse both lie: zero says the budget held, and one says it
// was broken. A workflow reads this one and decides for itself whether a
// detector may merge unchecked.
const (
	exitOK      = 0
	exitFinding = 1
	exitUsage   = 2
	exitSkipped = 3
)

// timeoutUsage is the one description of --request-timeout, which every
// command that reads a server carries.
const timeoutUsage = "how long one request waits for an answer; 0 waits forever"

// usage is what `feno` and `feno --help` print. The three commands are named
// with what each is for, because somebody who typed the bare command knows
// the tool exists and not yet what it does, and the flags are a command away.
const usage = `usage: feno <command> [flags]

  suggest   draft a detector from an SLI somebody already wrote
  generate  compile detectors into alert rules
  backtest  replay detectors over history against a noise budget
  version   the build this binary came from

feno <command> --help for the flags a command takes.
`

// printf writes diagnostic output. A write to stdout or stderr that fails
// leaves nowhere to report the failure, so the error is dropped rather than
// checked at every call site.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func main() {
	// The signal cancels the read in flight rather than killing the process,
	// so a run interrupted after seven of eight reports keeps the seven.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// parseFlags parses a command's flags and decides where the flag package's
// own output belongs. Help somebody asked for is output: it goes to stdout
// and the run succeeds, so `feno backtest --help | less` shows something. A
// usage error nobody asked for goes to stderr with a non-zero code.
//
// The flag package writes both of those the same way, through the flag set's
// output, so what it wrote is held in a buffer until Parse says which it was.
// The returned code is meaningful only when ok is false.
func parseFlags(fs *flag.FlagSet, args []string, line string, stdout, stderr io.Writer) (int, bool) {
	var held bytes.Buffer
	writeTo := func(w io.Writer) {
		fs.SetOutput(w)
		fs.Usage = func() {
			printf(w, "%s\n\n", line)
			fs.PrintDefaults()
		}
	}
	writeTo(&held)

	err := fs.Parse(args)

	// Every later fs.Usage() is the command rejecting its own arguments, so
	// from here the flag set writes to stderr like any other diagnostic.
	writeTo(stderr)

	switch {
	case err == nil:
		return exitOK, true
	case errors.Is(err, flag.ErrHelp):
		_, _ = held.WriteTo(stdout)
		return exitOK, false
	default:
		_, _ = held.WriteTo(stderr)
		return exitUsage, false
	}
}

// run holds the dispatch so a test can drive the whole binary without a
// process.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printf(stderr, "%s", usage)
		return exitUsage
	}

	switch args[0] {
	case "help", "-h", "--help":
		printf(stdout, "%s", usage)
		return exitOK
	case "generate":
		return generateCommand(ctx, args[1:], stdout, stderr)
	case "backtest":
		return backtestCommand(ctx, args[1:], stdout, stderr)
	case "suggest":
		return suggestCommand(ctx, args[1:], stdout, stderr)
	case "version":
		printf(stdout, "%s", buildinfo.Get())
		return exitOK
	default:
		printf(stderr, "unknown command %q, want suggest, generate, backtest or version\n", args[0])
		return exitUsage
	}
}
