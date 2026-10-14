package prometheus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// madProbeQuery is what the probe asks.
//
// `up` because every Prometheus-compatible server has it, and a one minute
// range because the probe is answered by the parser: a query over a metric
// that is absent still parses, and parsing is the whole question. The instant
// is now, so the query costs the server one evaluation of the shortest range
// anything is stored at.
const madProbeQuery = `mad_over_time(up[1m])`

// madFunctionName is the function the probe is about, and the name the
// parser's rejection quotes back.
const madFunctionName = "mad_over_time"

// Capabilities is the optional features this server turned out to have.
//
// It is a property of the deployment rather than of the dialect: Prometheus
// gates a set of functions behind --enable-feature=promql-experimental-functions,
// and Mimir, Thanos and VictoriaMetrics each decide separately whether to
// implement one at all. So the same specification compiles differently against
// two servers speaking the same PromQL, and only the server knows which.
type Capabilities struct {
	// ExperimentalFunctions is whether mad_over_time can be used in a rule
	// this server will evaluate.
	ExperimentalFunctions bool
}

// Probe asks the server which optional features it has.
//
// Three answers are told apart, and the third is why this returns an error at
// all. A server that answers the query has the function. A server that rejects
// it by name does not, which is a capability rather than a failure. Anything
// else, a proxy returning 503, a host that does not resolve, a rejection that
// does not name the function, is a failure and is reported as one: a band that
// quietly changed shape because something in the path was broken is the exact
// thing the provenance header exists to make visible.
func (c *Client) Probe(ctx context.Context) (Capabilities, error) {
	_, err := c.Query(ctx, madProbeQuery, time.Now())
	switch {
	case err == nil:
		return Capabilities{ExperimentalFunctions: true}, nil
	case functionUnavailable(err, madFunctionName):
		return Capabilities{}, nil
	default:
		return Capabilities{}, fmt.Errorf("probing %s for %s: %w", c.base, madFunctionName, err)
	}
}

// functionUnavailable reports whether the server rejected the query because
// the function is not one it will evaluate.
//
// Two sentences mean that, because the PromQL parser has two paths to it. A
// Prometheus that carries the function behind the feature flag knows the name
// and refuses it as not enabled. A parser that never had the function at all,
// which is Thanos, VictoriaMetrics and whichever Mimir has not caught up to
// the upstream function list, does not know the name. Either way the band has
// to be compiled the other way.
//
// The specific rejections, not any rejection: the same 400 covers a query
// over too many series or a range past a configured limit, and reading every
// 400 as "the function is absent" would compile the fallback band whenever
// the probe itself was malformed.
func functionUnavailable(err error, name string) bool {
	if !errors.Is(err, ErrQuery) {
		return false
	}
	message := err.Error()
	for _, rejection := range []string{
		fmt.Sprintf("function %q is not enabled", name),
		fmt.Sprintf("unknown function with name %q", name),
	} {
		if strings.Contains(message, rejection) {
			return true
		}
	}
	return false
}
