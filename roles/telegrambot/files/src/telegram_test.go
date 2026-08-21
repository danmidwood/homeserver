package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The whole API surface is exercised against a local server, so the tests need
// no network and no real bot token.
func fakeTelegram(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "test-token", HTTP: srv.Client()}
}

func TestGetUpdates(t *testing.T) {
	c := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/bottest-token/getUpdates") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		r.ParseForm()
		if r.FormValue("offset") != "7" {
			t.Errorf("offset not passed through: %q", r.FormValue("offset"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": []map[string]any{
				{"update_id": 7, "message": map[string]any{"text": "/status", "from": map[string]any{"id": 42}}},
			},
		})
	})
	ups, err := c.GetUpdates(context.Background(), 7, 0)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(ups) != 1 || ups[0].UpdateID != 7 || ups[0].Message.Text != "/status" {
		t.Fatalf("unexpected updates: %+v", ups)
	}
}

func TestSendMessageEscapesNothingItself(t *testing.T) {
	var got string
	c := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.FormValue("text")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	if err := c.SendMessage(context.Background(), 42, "already &amp; escaped"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// Callers escape before calling; double-escaping here would corrupt them.
	if got != "already &amp; escaped" {
		t.Errorf("SendMessage altered the text: %q", got)
	}
}

// A non-2xx or ok:false response must be an error, not silent success.
func TestAPIErrorIsReported(t *testing.T) {
	c := fakeTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "bad request"})
	})
	if err := c.SendMessage(context.Background(), 42, "hi"); err == nil {
		t.Error("an API error was reported as success")
	}
}
