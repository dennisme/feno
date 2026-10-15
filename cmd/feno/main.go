// Command feno compiles anomaly detector specifications into alert rules and
// replays them over history.
package main

import (
	"context"
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

// run holds the dispatch so a test can drive the whole binary without a
// process.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printf(stderr, "%s\n", "usage: feno <suggest|generate|backtest|version>")
		return exitUsage
	}

	switch args[0] {
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
