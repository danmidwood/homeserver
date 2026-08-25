package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseSpikeTime(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 8, 25, 14, 30, 0, 0, loc)

	tests := []struct {
		arg       string
		wantWorst bool
		want      time.Time
		wantErr   bool
	}{
		{arg: "", wantWorst: true},
		{arg: "23:00", want: time.Date(2026, 8, 25, 23, 0, 0, 0, loc)},
		{arg: "09:05", want: time.Date(2026, 8, 25, 9, 5, 0, 0, loc)},
		{arg: "yesterday 23:00", want: time.Date(2026, 8, 24, 23, 0, 0, 0, loc)},
		{arg: "2026-08-24 22:58", want: time.Date(2026, 8, 24, 22, 58, 0, 0, loc)},
		{arg: "not a time", wantErr: true},
		{arg: "25:99", wantErr: true},
	}

	for _, tc := range tests {
		got, worst, err := parseSpikeTime(tc.arg, now, loc)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSpikeTime(%q): expected an error, got %v", tc.arg, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSpikeTime(%q): %v", tc.arg, err)
			continue
		}
		if worst != tc.wantWorst {
			t.Errorf("parseSpikeTime(%q): findWorst = %v, want %v", tc.arg, worst, tc.wantWorst)
		}
		if !tc.wantWorst && !got.Equal(tc.want) {
			t.Errorf("parseSpikeTime(%q) = %v, want %v", tc.arg, got, tc.want)
		}
	}
}

// spikeServer answers each Prometheus query with canned data, so the renderer
// can be tested without a live server. The baseline is the same query an hour
// before the moment, so the two calls are told apart by the exact timestamp
// the caller asks for rather than by the order they arrive in.
func spikeServer(t *testing.T, arg string, containerCPU map[string]string, baseline map[string]string) *httptest.Server {
	t.Helper()
	moment, _, err := parseSpikeTime(arg, time.Now(), time.Local)
	if err != nil {
		t.Fatalf("spikeServer: %v", err)
	}
	baselineTS := strconv.FormatInt(moment.Add(-time.Hour).Unix(), 10)
	vector := func(w http.ResponseWriter, series []map[string]any) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": series},
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		at := r.URL.Query().Get("time")

		switch {
		case strings.Contains(q, "container_cpu_usage_seconds_total"):
			src := containerCPU
			if at == baselineTS {
				src = baseline
			}
			var out []map[string]any
			for name, v := range src {
				out = append(out, map[string]any{
					"metric": map[string]string{"name": name},
					"value":  []any{1.0, v},
				})
			}
			vector(w, out)
		case strings.Contains(q, "container_memory_usage_bytes"):
			vector(w, []map[string]any{
				{"metric": map[string]string{"name": "immich-server"}, "value": []any{1.0, "3328599654"}},
			})
		case strings.Contains(q, "node_load1"):
			vector(w, []map[string]any{{"metric": map[string]string{}, "value": []any{1.0, "20.9"}}})
		case strings.Contains(q, "hwmon"):
			vector(w, []map[string]any{{"metric": map[string]string{}, "value": []any{1.0, "99"}}})
		case strings.Contains(q, "MemAvailable"):
			vector(w, []map[string]any{{"metric": map[string]string{}, "value": []any{1.0, "71"}}})
		case strings.Contains(q, "ALERTS"):
			vector(w, []map[string]any{
				{"metric": map[string]string{"alertname": "SmartDiskTemperature", "severity": "warning"},
					"value": []any{1.0, "1"}},
			})
		case strings.Contains(q, "node_systemd_unit_state"):
			vector(w, []map[string]any{
				{"metric": map[string]string{"name": "restic-backup.service"}, "value": []any{1.0, "1"}},
			})
		default:
			vector(w, nil)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWhatspikedReportsBusiestContainers(t *testing.T) {
	srv := spikeServer(t, "2026-08-24 22:58",
		map[string]string{"immich-server": "4.12", "plex": "0.03"},
		map[string]string{"immich-server": "0.08", "plex": "0.02"})

	b := &Bot{PromURL: srv.URL, AlertURL: srv.URL}
	got := b.whatspiked(context.Background(), "2026-08-24 22:58")

	if !strings.Contains(got, "immich-server") {
		t.Errorf("busiest container missing from output: %q", got)
	}
	// 4.12 cores is 412% of one core.
	if !strings.Contains(got, "412") {
		t.Errorf("cpu percentage not rendered: %q", got)
	}
}

// The baseline is the point of the command: it distinguishes "busy" from
// "busy and was not an hour ago". Without it the reply cannot answer the
// question that was asked.
func TestWhatspikedShowsTheHourEarlierBaseline(t *testing.T) {
	srv := spikeServer(t, "2026-08-24 22:58",
		map[string]string{"immich-server": "4.12"},
		map[string]string{"immich-server": "0.08"})

	b := &Bot{PromURL: srv.URL, AlertURL: srv.URL}
	got := b.whatspiked(context.Background(), "2026-08-24 22:58")

	if !strings.Contains(got, "8%") {
		t.Errorf("baseline value missing: %q", got)
	}
}

// Container names are interpolated into an HTML message. A stray & or < makes
// Telegram reject the whole message, so an unescaped name means no reply.
func TestWhatspikedEscapesContainerNames(t *testing.T) {
	srv := spikeServer(t, "2026-08-24 22:58",
		map[string]string{"weird<&>name": "1.00"},
		map[string]string{})

	b := &Bot{PromURL: srv.URL, AlertURL: srv.URL}
	got := b.whatspiked(context.Background(), "2026-08-24 22:58")

	if strings.Contains(got, "weird<&>name") {
		t.Errorf("container name was not escaped: %q", got)
	}
	if !strings.Contains(got, "weird&lt;&amp;&gt;name") {
		t.Errorf("container name not escaped as expected: %q", got)
	}
}

// A dead Prometheus must produce a reply saying so, not an empty message.
func TestWhatspikedSurvivesPrometheusBeingDown(t *testing.T) {
	b := &Bot{PromURL: "http://127.0.0.1:1", AlertURL: "http://127.0.0.1:1"}
	got := b.whatspiked(context.Background(), "23:00")
	if got == "" {
		t.Error("whatspiked returned nothing when Prometheus was unreachable")
	}
}

// A bad argument should explain the accepted forms rather than fail silently
// or report a spike at some arbitrary time.
func TestWhatspikedRejectsUnparseableTime(t *testing.T) {
	b := &Bot{PromURL: "http://127.0.0.1:1", AlertURL: "http://127.0.0.1:1"}
	got := b.whatspiked(context.Background(), "half past nine")
	if !strings.Contains(got, "Usage") {
		t.Errorf("expected usage text for an unparseable time, got %q", got)
	}
}

func TestPromVectorAtReadsEverySeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{"resultType": "vector", "result": []map[string]any{
				{"metric": map[string]string{"name": "a"}, "value": []any{1.0, "1.5"}},
				{"metric": map[string]string{"name": "b"}, "value": []any{1.0, "2.5"}},
			}},
		})
	}))
	defer srv.Close()

	got, err := promVectorAt(context.Background(), srv.URL, "q", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("promVectorAt: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d series, want 2", len(got))
	}
	if got[0].Labels["name"] != "a" || got[0].Value != 1.5 {
		t.Errorf("first series wrong: %+v", got[0])
	}
}

// With no argument the command has to find the worst moment itself. That range
// query is the path most likely to be used from a phone -- an alert says
// something was wrong, not when.
func TestWhatspikedFindsTheWorstMomentItself(t *testing.T) {
	worst := time.Now().Add(-3 * time.Hour).Truncate(time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "query_range") {
			json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{"resultType": "matrix", "result": []map[string]any{{
					"metric": map[string]string{},
					"values": [][]any{
						{float64(worst.Add(-time.Hour).Unix()), "2.0"},
						{float64(worst.Unix()), "20.9"},
						{float64(worst.Add(time.Hour).Unix()), "3.0"},
					},
				}}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{"resultType": "vector", "result": []map[string]any{
				{"metric": map[string]string{}, "value": []any{1.0, "20.9"}},
			}},
		})
	}))
	defer srv.Close()

	b := &Bot{PromURL: srv.URL, AlertURL: srv.URL}
	got := b.whatspiked(context.Background(), "")

	if !strings.Contains(got, "Worst load in 24h") {
		t.Errorf("expected the worst-moment header, got %q", got)
	}
	if !strings.Contains(got, worst.Format("15:04")) {
		t.Errorf("expected the peak time %s in the reply, got %q", worst.Format("15:04"), got)
	}
}

// The peak must be the highest sample, not the first or the last one seen.
func TestPromWorstMomentPicksTheMaximum(t *testing.T) {
	base := time.Now().Truncate(time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{"resultType": "matrix", "result": []map[string]any{{
				"metric": map[string]string{},
				"values": [][]any{
					{float64(base.Unix()), "5.0"},
					{float64(base.Add(time.Minute).Unix()), "40.0"},
					{float64(base.Add(2 * time.Minute).Unix()), "6.0"},
				},
			}}},
		})
	}))
	defer srv.Close()

	got, err := promWorstMoment(context.Background(), srv.URL, "node_load1",
		base, base.Add(time.Hour), 5*time.Minute)
	if err != nil {
		t.Fatalf("promWorstMoment: %v", err)
	}
	if want := base.Add(time.Minute); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// An armed timer reports as "active" permanently, and docker and the bot
// itself always run. If they were listed, every reply would carry the same
// five names and the line would stop being read at all -- the same reasoning
// that drops the Watchdog from /status. The filter therefore has to happen in
// the query, so this asserts on what is actually sent to Prometheus.
func TestWhatspikedAsksOnlyForJobsThatWereRunning(t *testing.T) {
	var unitQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "node_systemd_unit_state") {
			unitQuery = q
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{"resultType": "vector", "result": []map[string]any{
				{"metric": map[string]string{}, "value": []any{1.0, "1"}},
			}},
		})
	}))
	defer srv.Close()

	b := &Bot{PromURL: srv.URL, AlertURL: srv.URL}
	b.whatspiked(context.Background(), "2026-08-24 22:58")

	if unitQuery == "" {
		t.Fatal("no systemd query was sent")
	}
	if !strings.Contains(unitQuery, `.service`) {
		t.Errorf("query does not restrict to services, so armed timers would be listed: %q", unitQuery)
	}
	for _, always := range []string{"docker", "telegram-bot"} {
		if !strings.Contains(unitQuery, always) {
			t.Errorf("query does not exclude the always-running unit %q: %q", always, unitQuery)
		}
	}
}

// The reply is a fixed-width table inside <pre>, so every cpu figure must
// start at the same column. trim() appends an ellipsis *after* cutting to n,
// returning n+1 characters, which pushed the number one column right for any
// name long enough to be truncated -- visible in the first real reply as
// "immich-machine-learn…   195%" sitting past "immich-server          190%".
func TestWhatspikedAlignsColumnsWhenNamesAreTruncated(t *testing.T) {
	srv := spikeServer(t, "2026-08-24 22:58",
		map[string]string{
			"immich-machine-learning": "1.95", // long enough to be truncated
			"immich-server":           "1.90",
		},
		map[string]string{"immich-machine-learning": "1.65", "immich-server": "1.82"})

	b := &Bot{PromURL: srv.URL, AlertURL: srv.URL}
	got := b.whatspiked(context.Background(), "2026-08-24 22:58")

	var cols []int
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "immich-") {
			continue
		}
		i := strings.Index(line, "%")
		if i < 0 {
			t.Fatalf("no percentage in row %q", line)
		}
		cols = append(cols, len([]rune(line[:i])))
	}
	if len(cols) < 2 {
		t.Fatalf("expected two container rows, got %d in %q", len(cols), got)
	}
	for _, c := range cols[1:] {
		if c != cols[0] {
			t.Errorf("cpu column not aligned: positions %v in\n%s", cols, got)
		}
	}
}

func TestPadName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"immich-server", "immich-server       "},
		{"immich-machine-learning", "immich-machine-lear…"},
		{"x", "x                   "},
	} {
		got := padName(tc.in, 20)
		if got != tc.want {
			t.Errorf("padName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if n := len([]rune(got)); n != 20 {
			t.Errorf("padName(%q) is %d columns, want 20", tc.in, n)
		}
	}
}

// Both numeric columns are right-aligned, so the digits line up under each
// other. The baseline started life as a bare "%s", which left-aligned it:
// "160%" and "1%" began at the same column instead of ending at one.
func TestWhatspikedRightAlignsBothNumericColumns(t *testing.T) {
	srv := spikeServer(t, "2026-08-24 22:58",
		map[string]string{"immich-machine-learning": "1.96", "planka": "0.01"},
		map[string]string{"immich-machine-learning": "1.60", "planka": "0.01"})

	b := &Bot{PromURL: srv.URL, AlertURL: srv.URL}
	got := b.whatspiked(context.Background(), "2026-08-24 22:58")

	var cpuEnds, baseEnds []int
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "immich-") && !strings.HasPrefix(line, "planka") {
			continue
		}
		r := []rune(line)
		var ends []int
		for i, c := range r {
			if c == '%' {
				ends = append(ends, i)
			}
		}
		if len(ends) != 2 {
			t.Fatalf("expected two percentages in %q, got %d", line, len(ends))
		}
		cpuEnds = append(cpuEnds, ends[0])
		baseEnds = append(baseEnds, ends[1])
	}
	if len(cpuEnds) < 2 {
		t.Fatalf("expected two rows, got %d in\n%s", len(cpuEnds), got)
	}
	for i := 1; i < len(cpuEnds); i++ {
		if cpuEnds[i] != cpuEnds[0] {
			t.Errorf("cpu column not right-aligned: %v in\n%s", cpuEnds, got)
		}
		if baseEnds[i] != baseEnds[0] {
			t.Errorf("baseline column not right-aligned: %v in\n%s", baseEnds, got)
		}
	}
}
