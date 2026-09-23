package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var resultsCmd = &cobra.Command{
	Use:   "results <run-id>",
	Short: "Show benchmark results for a completed run",
	Long: `results queries VictoriaMetrics for the metrics tagged with the given run
ID and renders a per-transaction summary table. Set metrics.endpoint first, with
'benchctl config set metrics.endpoint <url>' or BENCHCTL_METRICS_ENDPOINT.`,
	Args: cobra.ExactArgs(1),
	RunE: showResults,
}

func showResults(cmd *cobra.Command, args []string) error {
	runID := args[0]

	endpoint := appCfg.Metrics.Endpoint
	if endpoint == "" {
		return fmt.Errorf("VictoriaMetrics endpoint required: set metrics.endpoint or BENCHCTL_METRICS_ENDPOINT")
	}
	token := appCfg.Metrics.Token
	username := appCfg.Metrics.Username
	password := appCfg.Metrics.Password

	results, err := queryRun(cmd.Context(), endpoint, token, username, password, runID)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return fmt.Errorf("no metrics found for run %s", runID)
	}

	renderResults(os.Stdout, runID, results)
	return nil
}

// vmSample holds one instant-query result from VictoriaMetrics.
type vmSample struct {
	Metric map[string]string  `json:"metric"`
	Value  [2]json.RawMessage `json:"value"` // [timestamp_float, "value_string"]
}

// queryRun fetches all metrics for a run ID using last_over_time over a 2-year
// window so the query works regardless of when the run completed.
func queryRun(ctx context.Context, endpoint, token, username, password, runID string) ([]vmSample, error) {
	query := fmt.Sprintf(`last_over_time({run_id=%q}[730d])`, runID)
	u, err := url.Parse(strings.TrimRight(endpoint, "/") + "/api/v1/query")
	if err != nil {
		return nil, fmt.Errorf("results: invalid endpoint: %w", err)
	}
	u.RawQuery = url.Values{"query": {query}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("results: build request: %w", err)
	}
	switch {
	case token != "":
		req.Header.Set("Authorization", "Bearer "+token)
	case username != "":
		req.SetBasicAuth(username, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("results: query failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("results: read response: %w", err)
	}

	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result []vmSample `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("results: parse response: %w", err)
	}
	if envelope.Status != "success" {
		return nil, fmt.Errorf("results: VM query error: %s", envelope.Error)
	}
	return envelope.Data.Result, nil
}

// txStats holds the per-transaction metrics for the results table.
type txStats struct {
	TPM, P50, P90, P99, Max, Avg, Count, Duration float64
	hasLatency                                    bool
}

type benchmarkGroup struct {
	infoLabels map[string]string
	ok         map[string]*txStats
	errMap     map[string]*txStats
}

func renderResults(w io.Writer, runID string, samples []vmSample) {
	// Group all samples by the "benchmark" label so that a multi-benchmark run
	// (e.g. one engine then another) renders as separate tables rather than one
	// table where later values silently overwrite earlier ones.
	groups := make(map[string]*benchmarkGroup)
	var benchOrder []string

	for _, s := range samples {
		bm := s.Metric["benchmark"]
		if _, exists := groups[bm]; !exists {
			groups[bm] = &benchmarkGroup{
				infoLabels: make(map[string]string),
				ok:         make(map[string]*txStats),
				errMap:     make(map[string]*txStats),
			}
			benchOrder = append(benchOrder, bm)
		}
		g := groups[bm]

		name := s.Metric["__name__"]
		val := sampleFloat(s)

		if name == "benchctl_run_info" {
			for k, v := range s.Metric {
				if k != "__name__" && k != "run_id" && k != "benchmark" {
					g.infoLabels[k] = v
				}
			}
			continue
		}

		tx := s.Metric["transaction"]
		status := s.Metric["status"]
		if tx == "" {
			continue
		}

		dst := g.ok
		if status == "error" {
			dst = g.errMap
		}
		if dst[tx] == nil {
			dst[tx] = &txStats{}
		}
		row := dst[tx]

		switch name {
		case "tpcc_tpm":
			row.TPM = val
		case "tpcc_count":
			row.Count = val
		case "tpcc_duration_seconds":
			row.Duration = val
		case "tpcc_latency_ms":
			row.hasLatency = true
			switch s.Metric["quantile"] {
			case "p50":
				row.P50 = val
			case "p90":
				row.P90 = val
			case "p99":
				row.P99 = val
			case "max":
				row.Max = val
			case "avg":
				row.Avg = val
			}
		}
	}

	sort.Strings(benchOrder)
	fmt.Fprintf(w, "Run: %s\n", runID)

	for _, bm := range benchOrder {
		g := groups[bm]
		fmt.Fprintln(w)
		if bm != "" {
			fmt.Fprintf(w, "benchmark: %s\n", bm)
		}
		if len(g.infoLabels) > 0 {
			keys := sortedKeys(g.infoLabels)
			for _, k := range keys {
				fmt.Fprintf(w, "%s: %s\n", k, g.infoLabels[k])
			}
		}
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		printTable(tw, g.ok)
		if len(g.errMap) > 0 {
			fmt.Fprintln(tw)
			fmt.Fprintln(tw, "Errors:")
			printTable(tw, g.errMap)
		}
		tw.Flush()
	}
}

func printTable(w io.Writer, rows map[string]*txStats) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, "Transaction\tTPM\tp50 ms\tp90 ms\tp99 ms\tmax ms\tavg ms\tCount\tDuration")
	txNames := sortedKeys(rows)
	for _, tx := range txNames {
		r := rows[tx]
		dur := ""
		if r.Duration > 0 {
			dur = strconv.FormatFloat(r.Duration, 'f', 0, 64) + "s"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			tx,
			fmtF(r.TPM),
			fmtF(r.P50),
			fmtF(r.P90),
			fmtF(r.P99),
			fmtF(r.Max),
			fmtF(r.Avg),
			fmtF(r.Count),
			dur,
		)
	}
}

func fmtF(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func sampleFloat(s vmSample) float64 {
	if len(s.Value[1]) == 0 {
		return 0
	}
	var str string
	if err := json.Unmarshal(s.Value[1], &str); err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(str, 64)
	return v
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
