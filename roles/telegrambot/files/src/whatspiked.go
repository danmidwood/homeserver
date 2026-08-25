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

// Explaining a load, temperature or memory spike from a phone.
//
// This is the bot-side counterpart of tools/whatspiked. It deliberately asks
// Prometheus for everything rather than reading the journal: the bot's sudoers
// grants exact commands with no wildcards, and a journal query with a variable
// time window cannot be expressed that way. The systemd collector already
// records which of our units were active, so the same question is answered
// from data we are collecting anyway.

// promSeries is one labelled sample from an instant query.
type promSeries struct {
	Labels map[string]string
	Value  float64
}

// promVectorAt runs an instant query at a point in time and returns every
// series it produced. Unlike promScalar an empty result is not an error here:
// "no containers were busy" is a legitimate answer to some of these queries.
func promVectorAt(ctx context.Context, base, query string, at time.Time) ([]promSeries, error) {
	v := url.Values{"query": {query}}
	if !at.IsZero() {
		v.Set("time", strconv.FormatInt(at.Unix(), 10))
	}
	endpoint := strings.TrimRight(base, "/") + "/api/v1/query?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("query %q: prometheus returned status %q", query, out.Status)
	}

	series := make([]promSeries, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		if len(r.Value) < 2 {
			continue
		}
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		series = append(series, promSeries{Labels: r.Metric, Value: f})
	}
	return series, nil
}

// promWorstMoment returns the time at which query was highest over a range.
func promWorstMoment(ctx context.Context, base, query string, start, end time.Time, step time.Duration) (time.Time, error) {
	v := url.Values{
		"query": {query},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.Itoa(int(step.Seconds()))},
	}
	endpoint := strings.TrimRight(base, "/") + "/api/v1/query_range?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()

	var out struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Values [][]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return time.Time{}, err
	}
	if out.Status != "success" || len(out.Data.Result) == 0 {
		return time.Time{}, fmt.Errorf("query %q returned no range data", query)
	}

	best, bestAt := 0.0, time.Time{}
	for _, r := range out.Data.Result {
		for _, pair := range r.Values {
			if len(pair) < 2 {
				continue
			}
			ts, ok := pair[0].(float64)
			if !ok {
				continue
			}
			s, ok := pair[1].(string)
			if !ok {
				continue
			}
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				continue
			}
			if bestAt.IsZero() || f > best {
				best, bestAt = f, time.Unix(int64(ts), 0)
			}
		}
	}
	if bestAt.IsZero() {
		return time.Time{}, fmt.Errorf("query %q produced no samples", query)
	}
	return bestAt, nil
}

// parseSpikeTime interprets the command argument. An empty argument means
// "find the worst moment yourself", which is the common case from a phone:
// the alert says something was wrong, not when.
func parseSpikeTime(arg string, now time.Time, loc *time.Location) (time.Time, bool, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return time.Time{}, true, nil
	}

	day := now.In(loc)
	if rest, ok := cutPrefixFold(arg, "yesterday "); ok {
		day = day.AddDate(0, 0, -1)
		arg = strings.TrimSpace(rest)
	}

	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, arg, loc); err == nil {
			return t, false, nil
		}
	}
	if t, err := time.ParseInLocation("15:04", arg, loc); err == nil {
		return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, loc), false, nil
	}
	return time.Time{}, false, fmt.Errorf("could not understand %q as a time", arg)
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// firstValue pulls the single value out of a one-series result.
func firstValue(series []promSeries, err error) (float64, bool) {
	if err != nil || len(series) == 0 {
		return 0, false
	}
	return series[0].Value, true
}

func (b *Bot) whatspiked(ctx context.Context, arg string) string {
	loc := time.Local
	moment, findWorst, err := parseSpikeTime(arg, time.Now(), loc)
	if err != nil {
		return "Usage: /whatspiked [HH:MM | yesterday HH:MM | YYYY-MM-DD HH:MM]\n" +
			"With no argument, explains the worst load in the last 24 hours."
	}

	var header string
	if findWorst {
		end := time.Now()
		moment, err = promWorstMoment(ctx, b.PromURL, "node_load1", end.Add(-24*time.Hour), end, 5*time.Minute)
		if err != nil {
			return "❓ Prometheus did not answer, so there is nothing to explain."
		}
		header = "Worst load in 24h — " + moment.In(loc).Format("2 Jan 15:04")
	} else {
		header = moment.In(loc).Format("2 Jan 15:04")
	}

	var body strings.Builder

	// Host line. Each value is omitted rather than guessed if it is missing.
	var host []string
	if v, ok := firstValue(promVectorAt(ctx, b.PromURL, "node_load1", moment)); ok {
		host = append(host, fmt.Sprintf("load %.1f", v))
	}
	if v, ok := firstValue(promVectorAt(ctx, b.PromURL,
		`max(node_hwmon_temp_celsius{chip=~".*coretemp.*"})`, moment)); ok {
		host = append(host, fmt.Sprintf("cpu %.0fC", v))
	}
	if v, ok := firstValue(promVectorAt(ctx, b.PromURL,
		`100*(1-node_memory_MemAvailable_bytes/node_memory_MemTotal_bytes)`, moment)); ok {
		host = append(host, fmt.Sprintf("mem %.0f%%", v))
	}
	if len(host) == 0 {
		return "❓ Prometheus did not answer for " + html.EscapeString(header) + "."
	}
	body.WriteString(strings.Join(host, "   ") + "\n")

	// Busiest containers, with the same figure an hour earlier beside it. The
	// comparison is the whole point: it separates "busy" from "unusually busy".
	const cpuQuery = `rate(container_cpu_usage_seconds_total{name!=""}[5m])`
	now, nowErr := promVectorAt(ctx, b.PromURL, cpuQuery, moment)
	before, _ := promVectorAt(ctx, b.PromURL, cpuQuery, moment.Add(-time.Hour))

	baseline := map[string]float64{}
	for _, s := range before {
		baseline[s.Labels["name"]] = s.Value
	}

	if nowErr == nil && len(now) > 0 {
		sort.Slice(now, func(i, j int) bool { return now[i].Value > now[j].Value })
		if len(now) > 6 {
			now = now[:6]
		}
		body.WriteString("\ncontainer              cpu   1h before\n")
		for _, s := range now {
			name := s.Labels["name"]
			was := "—"
			if v, ok := baseline[name]; ok {
				was = fmt.Sprintf("%.0f%%", v*100)
			}
			// Truncate and pad on the raw name, then escape. Doing it the
			// other way round can cut an entity in half ("&am"), and padding
			// an escaped string misaligns the column because Telegram renders
			// "&amp;" back to a single character.
			body.WriteString(html.EscapeString(fmt.Sprintf("%-20s", trim(name, 20))) +
				fmt.Sprintf(" %5.0f%%   %s\n", s.Value*100, was))
		}
	}

	if mem, err := promVectorAt(ctx, b.PromURL,
		`container_memory_usage_bytes{name!=""}`, moment); err == nil && len(mem) > 0 {
		sort.Slice(mem, func(i, j int) bool { return mem[i].Value > mem[j].Value })
		var parts []string
		for _, s := range mem[:min(3, len(mem))] {
			parts = append(parts, fmt.Sprintf("%s %.1fG",
				html.EscapeString(s.Labels["name"]), s.Value/1073741824))
		}
		body.WriteString("\nmemory  " + strings.Join(parts, ", ") + "\n")
	}

	// Which of our scheduled jobs were running then. This replaces the CLI
	// tool's journal scrape, which the bot cannot do without a sudo wildcard.
	//
	// Timers are excluded because an armed timer is always "active", and
	// docker and the bot itself are excluded because they always run. Listing
	// them would put the same five names on every reply and train the reader
	// to skip the line -- the same reason the Watchdog is dropped from
	// /status. What is left is work that was genuinely running at the time.
	const unitQuery = `node_systemd_unit_state{state="active",` +
		`name=~".*\\.service",name!~"(docker|telegram-bot)\\.service"} == 1`
	if units, err := promVectorAt(ctx, b.PromURL, unitQuery, moment); err == nil && len(units) > 0 {
		var names []string
		for _, s := range units {
			n := strings.TrimSuffix(s.Labels["name"], ".service")
			if n != "" {
				names = append(names, html.EscapeString(n))
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			body.WriteString("active  " + strings.Join(names, ", ") + "\n")
		}
	}

	if alerts, err := promVectorAt(ctx, b.PromURL,
		`ALERTS{alertstate="firing"}`, moment); err == nil {
		var names []string
		seen := map[string]bool{}
		for _, s := range alerts {
			n := s.Labels["alertname"]
			if n == "" || n == "Watchdog" || seen[n] {
				continue
			}
			seen[n] = true
			names = append(names, fmt.Sprintf("%s (%s)",
				html.EscapeString(n), html.EscapeString(s.Labels["severity"])))
		}
		sort.Strings(names)
		if len(names) > 0 {
			body.WriteString("alerts  " + strings.Join(names, ", ") + "\n")
		}
	}

	return "<b>" + html.EscapeString(header) + "</b>\n<pre>" + body.String() + "</pre>"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
