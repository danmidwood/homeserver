package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// promScalar runs an instant query and returns the single sample's value.
//
// An empty result is an error, deliberately. Prometheus returns an empty vector
// when a metric does not exist, and rendering that as 0 would turn "cAdvisor is
// gone" into "0 containers running" -- a confident wrong answer instead of an
// honest failure.
func promScalar(ctx context.Context, base, query string) (float64, error) {
	endpoint := strings.TrimRight(base, "/") + "/api/v1/query?" + url.Values{"query": {query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var out struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("query %q: prometheus returned status %q", query, out.Status)
	}
	if len(out.Data.Result) == 0 || len(out.Data.Result[0].Value) < 2 {
		return 0, fmt.Errorf("query %q returned no samples", query)
	}
	s, ok := out.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("query %q: unexpected value type", query)
	}
	return strconv.ParseFloat(s, 64)
}

// Returns the rendered names and the TOTAL number of firing alerts, which are
// different numbers: six ImageUpdateAvailable alerts are one name. Reporting the
// name count as though it were the alert count produced "1 firing: X ×6".
func firingAlertNames(ctx context.Context, base string) ([]string, int, error) {
	endpoint := strings.TrimRight(base, "/") + "/api/v2/alerts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	var alerts []struct {
		Labels map[string]string `json:"labels"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		return nil, 0, err
	}

	counts := map[string]int{}
	total := 0
	for _, a := range alerts {
		if a.Status.State != "active" {
			continue
		}
		name := a.Labels["alertname"]
		// The Watchdog fires permanently by design; reporting it as a problem
		// every time would train the reader to ignore this line.
		if name == "" || name == "Watchdog" {
			continue
		}
		counts[name]++
		total++
	}

	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, n := range names {
		if counts[n] > 1 {
			out = append(out, fmt.Sprintf("%s ×%d", html.EscapeString(n), counts[n]))
		} else {
			out = append(out, html.EscapeString(n))
		}
	}
	return out, total, nil
}

func (b *Bot) status(ctx context.Context) string {
	var lines []string

	up, upErr := promScalar(ctx, b.PromURL, `count(container_last_seen{name!=""})`)
	expected, expErr := promScalar(ctx, b.PromURL, `count(container_expected)`)
	switch {
	case upErr != nil || expErr != nil:
		lines = append(lines, "❓ containers: Prometheus did not answer")
	case up < expected:
		lines = append(lines, fmt.Sprintf("⚠️ %.0f/%.0f containers up", up, expected))
	default:
		lines = append(lines, fmt.Sprintf("✅ %.0f/%.0f containers up", up, expected))
	}

	var disk []string
	for _, mp := range []string{"/", "/var", "/mnt/storage"} {
		q := fmt.Sprintf(`100*(1-node_filesystem_avail_bytes{mountpoint=%q}/node_filesystem_size_bytes{mountpoint=%q})`, mp, mp)
		if v, err := promScalar(ctx, b.PromURL, q); err == nil {
			disk = append(disk, fmt.Sprintf("%s %.0f%%", html.EscapeString(mp), v))
		}
	}
	if len(disk) > 0 {
		lines = append(lines, "💾 "+strings.Join(disk, "   "))
	}

	if hrs, err := promScalar(ctx, b.PromURL, `(time()-restic_backup_last_success_timestamp_seconds)/3600`); err == nil {
		lines = append(lines, fmt.Sprintf("🕒 backup %.0fh ago", hrs))
	} else {
		lines = append(lines, "❓ backup: no timestamp recorded")
	}

	if names, total, err := firingAlertNames(ctx, b.AlertURL); err != nil {
		lines = append(lines, "❓ alerts: Alertmanager did not answer")
	} else if total == 0 {
		lines = append(lines, "✅ no alerts firing")
	} else {
		lines = append(lines, fmt.Sprintf("⚠️ %d firing: %s", total, strings.Join(names, ", ")))
	}

	return strings.Join(lines, "\n")
}
