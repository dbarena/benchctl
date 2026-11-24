package pgbench

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dbarena/benchctl/internal/engine"
)

// summary holds the figures pgbench prints after a run. Optional lines are
// absent from older versions or without the options that produce them, and
// are tracked so that no zero is reported for a figure pgbench never gave.
type summary struct {
	tps          float64
	transactions float64

	failed, latencyAvgMs, latencyStddevMs, connTimeMs, durationS         float64
	hasFailed, hasLatencyAvg, hasLatencyStddev, hasConnTime, hasDuration bool
}

var (
	// "number of transactions actually processed: 50464", or "1000/1000" when
	// run with --transactions.
	processedRe = regexp.MustCompile(`^number of transactions actually processed: (\d+)`)
	// pgbench 15+.
	failedRe        = regexp.MustCompile(`^number of failed transactions: (\d+)`)
	latencyAvgRe    = regexp.MustCompile(`^latency average = ([0-9.]+) ms`)
	latencyStddevRe = regexp.MustCompile(`^latency stddev = ([0-9.]+) ms`)
	// pgbench 14+.
	connTimeRe = regexp.MustCompile(`^initial connection time = ([0-9.]+) ms`)
	durationRe = regexp.MustCompile(`^duration: (\d+) s`)
	// pgbench 14+ prints one tps line, "(without initial connection time)";
	// older versions print both "(including connections establishing)" and
	// "(excluding connections establishing)". The excluding figure is the
	// one comparable to the newer line.
	tpsRe = regexp.MustCompile(`^tps = ([0-9.]+) \((without initial connection time|excluding connections establishing|including connections establishing)\)`)
)

// parseSummary reads pgbench's closing summary from its output.
func parseSummary(out []byte) (summary, error) {
	var s summary
	var hasProcessed, hasTPS bool
	tpsRank := 0

	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := processedRe.FindStringSubmatch(line); m != nil {
			s.transactions = mustFloat(m[1])
			hasProcessed = true
		} else if m := failedRe.FindStringSubmatch(line); m != nil {
			s.failed, s.hasFailed = mustFloat(m[1]), true
		} else if m := latencyAvgRe.FindStringSubmatch(line); m != nil {
			s.latencyAvgMs, s.hasLatencyAvg = mustFloat(m[1]), true
		} else if m := latencyStddevRe.FindStringSubmatch(line); m != nil {
			s.latencyStddevMs, s.hasLatencyStddev = mustFloat(m[1]), true
		} else if m := connTimeRe.FindStringSubmatch(line); m != nil {
			s.connTimeMs, s.hasConnTime = mustFloat(m[1]), true
		} else if m := durationRe.FindStringSubmatch(line); m != nil {
			s.durationS, s.hasDuration = mustFloat(m[1]), true
		} else if m := tpsRe.FindStringSubmatch(line); m != nil {
			// Prefer the figure without connection setup when both appear.
			rank := 1
			if m[2] != "including connections establishing" {
				rank = 2
			}
			if rank >= tpsRank {
				s.tps, tpsRank = mustFloat(m[1]), rank
			}
			hasTPS = true
		}
	}
	if !hasProcessed || !hasTPS {
		return summary{}, fmt.Errorf("no run summary in pgbench output")
	}
	return s, nil
}

// structured translates the summary into pgbench_* metric families.
func (s summary) structured() engine.StructuredMetrics {
	var sm engine.StructuredMetrics
	point := func(family string, value float64, labels map[string]string) {
		if labels == nil {
			labels = map[string]string{}
		}
		sm.Points = append(sm.Points, engine.MetricPoint{Family: family, Labels: labels, Value: value})
	}
	point("pgbench_tps", s.tps, nil)
	point("pgbench_transactions", s.transactions, nil)
	if s.hasFailed {
		point("pgbench_failed_transactions", s.failed, nil)
	}
	if s.hasLatencyAvg {
		point("pgbench_latency_ms", s.latencyAvgMs, map[string]string{"stat": "avg"})
	}
	if s.hasLatencyStddev {
		point("pgbench_latency_ms", s.latencyStddevMs, map[string]string{"stat": "stddev"})
	}
	if s.hasConnTime {
		point("pgbench_connection_time_ms", s.connTimeMs, nil)
	}
	if s.hasDuration {
		point("pgbench_duration_seconds", s.durationS, nil)
	}
	return sm
}

func mustFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// progressRe matches a --progress report, e.g.
//
//	progress: 1.0 s, 16520.0 tps, lat 0.241 ms stddev 0.093, 0 failed
//
// The failed count is pgbench 15+; rate-limited runs append lag and skipped
// counts, which are not recorded.
var progressRe = regexp.MustCompile(`^progress: ([0-9.]+) s, ([0-9.]+) tps, lat ([^ ]+) ms stddev ([^ ,]+)(?:, (\d+) failed)?`)

// progressHeader is the first line of raw_samples_csv.
const progressHeader = "elapsed_s,tps,latency_avg_ms,latency_stddev_ms,failed"

// progressWriter collects pgbench's --progress reports, one CSV row each, from
// the output stream as it goes by. It keeps whole lines only, so a report split
// across two writes is still recognized.
type progressWriter struct {
	partial []byte
	rows    []string
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.partial = append(p.partial, b...)
	for {
		i := bytes.IndexByte(p.partial, '\n')
		if i < 0 {
			break
		}
		p.addLine(string(p.partial[:i]))
		p.partial = p.partial[i+1:]
	}
	return len(b), nil
}

func (p *progressWriter) addLine(line string) {
	m := progressRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return
	}
	failed := m[5]
	if failed == "" {
		failed = "0"
	}
	p.rows = append(p.rows, strings.Join([]string{m[1], m[2], m[3], m[4], failed}, ","))
}

// csv returns the collected reports as CSV, or "" if there were none.
func (p *progressWriter) csv() string {
	if len(p.rows) == 0 {
		return ""
	}
	return progressHeader + "\n" + strings.Join(p.rows, "\n") + "\n"
}
