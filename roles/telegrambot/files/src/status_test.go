package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func promServer(t *testing.T, value string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []map[string]any{
					{"metric": map[string]string{}, "value": []any{1.0, value}},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPromScalar(t *testing.T) {
	srv := promServer(t, "18")
	got, err := promScalar(context.Background(), srv.URL, "count(up)")
	if err != nil {
		t.Fatalf("promScalar: %v", err)
	}
	if got != 18 {
		t.Errorf("got %v, want 18", got)
	}
}

// An empty result is not zero -- it means the metric is absent, which is a
// different thing and must not be rendered as "0 containers up".
func TestPromScalarEmptyResultIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": []any{}},
		})
	}))
	defer srv.Close()
	if _, err := promScalar(context.Background(), srv.URL, "absent_metric"); err == nil {
		t.Error("an empty result was not reported as an error")
	}
}

// Alert names come from Prometheus and are interpolated into an HTML message.
// A stray & or < makes Telegram drop the whole message, so escaping is not
// cosmetic -- an unescaped name means no status message at all.
func TestStatusEscapesAlertNames(t *testing.T) {
	prom := promServer(t, "18")
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"labels": map[string]string{"alertname": "Weird<&>Name"},
				"status": map[string]any{"state": "active"}},
		})
	}))
	defer am.Close()

	b := &Bot{PromURL: prom.URL, AlertURL: am.URL, TG: &Client{HTTP: http.DefaultClient}}
	got := b.status(context.Background())

	if strings.Contains(got, "Weird<&>Name") {
		t.Errorf("alert name was not escaped: %q", got)
	}
	if !strings.Contains(got, "Weird&lt;&amp;&gt;Name") {
		t.Errorf("alert name was not escaped as expected: %q", got)
	}
}

// A dead Prometheus must produce a status message saying so, not an empty
// reply and not a panic.
func TestStatusSurvivesPrometheusBeingDown(t *testing.T) {
	b := &Bot{PromURL: "http://127.0.0.1:1", AlertURL: "http://127.0.0.1:1", TG: &Client{HTTP: http.DefaultClient}}
	got := b.status(context.Background())
	if got == "" {
		t.Error("status returned nothing when Prometheus was unreachable")
	}
}

// The Watchdog fires permanently by design. Listing it every time would train
// the reader to ignore the alerts line.
func TestStatusIgnoresWatchdog(t *testing.T) {
	prom := promServer(t, "18")
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"labels": map[string]string{"alertname": "Watchdog"},
				"status": map[string]any{"state": "active"}},
		})
	}))
	defer am.Close()

	b := &Bot{PromURL: prom.URL, AlertURL: am.URL, TG: &Client{HTTP: http.DefaultClient}}
	got := b.status(context.Background())
	if strings.Contains(got, "Watchdog") {
		t.Errorf("Watchdog was reported as a firing alert: %q", got)
	}
	if !strings.Contains(got, "no alerts firing") {
		t.Errorf("with only Watchdog active, status should say nothing is firing: %q", got)
	}
}

// Six alerts of one name are six firing alerts, not one. The first live run
// rendered "1 firing: ImageUpdateAvailable x6", which contradicts itself.
func TestStatusCountsAlertsNotNames(t *testing.T) {
	prom := promServer(t, "18")
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for i := 0; i < 6; i++ {
			out = append(out, map[string]any{
				"labels": map[string]string{"alertname": "ImageUpdateAvailable"},
				"status": map[string]any{"state": "active"},
			})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer am.Close()

	b := &Bot{PromURL: prom.URL, AlertURL: am.URL, TG: &Client{HTTP: http.DefaultClient}}
	got := b.status(context.Background())

	if !strings.Contains(got, "6 firing") {
		t.Errorf("expected the alert total, got %q", got)
	}
	if strings.Contains(got, "1 firing") {
		t.Errorf("reported the name count as the alert count: %q", got)
	}
}

// A backup that has just run rendered as "0h ago", which reads like a missing
// value rather than a fresh success -- the one case where the reader most
// wants confirmation that it worked.
func TestHumanAge(t *testing.T) {
	for _, tc := range []struct {
		seconds float64
		want    string
	}{
		{0, "just now"},
		{45, "just now"},
		{60, "1m ago"},
		{90, "1m ago"},
		{600, "10m ago"},
		{3599, "59m ago"},
		{3600, "1h ago"},
		{7200, "2h ago"},
		{47 * 3600, "47h ago"},
		{48 * 3600, "2d ago"},
		{72 * 3600, "3d ago"},
	} {
		if got := humanAge(tc.seconds); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

// A clock skew or a timestamp from the future must not render as a negative
// age, which would look like a bug in the bot rather than in the data.
func TestHumanAgeHandlesFutureTimestamps(t *testing.T) {
	if got := humanAge(-120); got != "just now" {
		t.Errorf("humanAge(-120) = %q, want %q", got, "just now")
	}
}
