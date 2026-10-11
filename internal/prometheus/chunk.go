package prometheus

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrRange reports a range that cannot be queried, which is an end before
	// its start.
	ErrRange = errors.New("end must not be before start")

	// ErrStep reports a step that cannot describe an evaluation interval.
	ErrStep = errors.New("step must be positive")
)

// maxPointsPerSeries is the resolution limit Prometheus enforces on a range
// query. The server compares end.Sub(start)/step against 11000 and refuses the
// query outright, so a thirty day backtest at a one minute step is several
// requests whether or not the caller wants it to be. The comparison is on
// intervals rather than points, so a chunk at the limit returns one more sample
// than the number here.
const maxPointsPerSeries = 11000

// chunk is one range query's worth of a larger range.
type chunk struct {
	start time.Time
	end   time.Time
}

// planChunks splits a range into requests no server will refuse.
//
// Two properties make the results stitch back into one series. Chunks are a
// step apart rather than adjacent, because query_range includes its end
// timestamp and a chunk starting where the last one ended would serve that
// boundary sample twice. And every chunk start is start plus a whole number of
// steps, because a series whose points do not all sit on one grid holds points
// that no evaluation interval explains.
//
// The range is truncated to the grid: Prometheus evaluates at start and then
// every step, so a range whose length is not a multiple of the step has no
// sample in its final remainder and asking for one would invent it.
func planChunks(start, end time.Time, step time.Duration, maxPoints int) ([]chunk, error) {
	if step <= 0 {
		return nil, fmt.Errorf("step %v: %w", step, ErrStep)
	}
	if end.Before(start) {
		return nil, fmt.Errorf("range %s..%s: %w", start, end, ErrRange)
	}
	if maxPoints <= 0 {
		maxPoints = maxPointsPerSeries
	}

	// The last timestamp on the grid that the range actually covers. Every
	// chunk end below is derived from start, which is what keeps the grid.
	last := start.Add(end.Sub(start) / step * step)
	width := time.Duration(maxPoints) * step

	var out []chunk
	for s := start; !s.After(last); s = s.Add(width + step) {
		e := s.Add(width)
		if e.After(last) {
			e = last
		}
		out = append(out, chunk{start: s, end: e})
	}
	return out, nil
}
