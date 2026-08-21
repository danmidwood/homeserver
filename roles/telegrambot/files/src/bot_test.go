package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	cases := []struct{ in, cmd, arg string }{
		{"/status", "/status", ""},
		{"/status@myhomebot", "/status", ""},
		{"/restart kavita", "/restart", "kavita"},
		{"/restart@myhomebot kavita", "/restart", "kavita"},
		{"  /backup_now  ", "/backup_now", ""},
		{"/restart kavita extra", "/restart", "kavita extra"},
		{"hello", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		cmd, arg := parseCommand(c.in)
		if cmd != c.cmd || arg != c.arg {
			t.Errorf("parseCommand(%q) = (%q,%q), want (%q,%q)", c.in, cmd, arg, c.cmd, c.arg)
		}
	}
}

// Authorisation is the security boundary for every command, so it is tested
// directly rather than only through the dispatch path.
func TestAuthorized(t *testing.T) {
	b := &Bot{AllowedUserID: 42}
	if !b.authorized(&Message{From: &User{ID: 42}}) {
		t.Error("the allowed user was rejected")
	}
	if b.authorized(&Message{From: &User{ID: 43}}) {
		t.Error("a different user id was accepted")
	}
	if b.authorized(&Message{From: nil}) {
		t.Error("a message with no sender was accepted")
	}
	if b.authorized(&Message{From: &User{ID: 0}}) {
		t.Error("a zero user id was accepted")
	}
}

// A zero AllowedUserID must never mean "allow everyone". Misconfiguration has
// to fail closed.
func TestAuthorizedFailsClosedWhenUnset(t *testing.T) {
	b := &Bot{AllowedUserID: 0}
	if b.authorized(&Message{From: &User{ID: 0}}) {
		t.Error("an unconfigured bot accepted a message")
	}
	if b.authorized(&Message{From: &User{ID: 99}}) {
		t.Error("an unconfigured bot accepted a message")
	}
}

// Telegram redelivers unacknowledged updates for 24 hours. If the offset is
// not persisted, a crash loop replays the commands in them -- repeatedly
// restarting containers or firing backups. This is the most dangerous failure
// mode in the design, so it has a test.
func TestOffsetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "offset")

	if got := loadOffset(path); got != 0 {
		t.Errorf("missing file should give offset 0, got %d", got)
	}
	if err := saveOffset(path, 1234); err != nil {
		t.Fatalf("saveOffset: %v", err)
	}
	if got := loadOffset(path); got != 1234 {
		t.Errorf("offset round trip gave %d, want 1234", got)
	}
	if err := os.WriteFile(path, []byte("not a number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadOffset(path); got != 0 {
		t.Errorf("corrupt file should give offset 0, got %d", got)
	}
}

func TestHandleUnknownCommandGivesHelp(t *testing.T) {
	b := &Bot{AllowedUserID: 42}
	got := b.Handle(context.Background(), &Message{From: &User{ID: 42}, Text: "/nonsense"})
	if got == "" {
		t.Error("an unknown command produced no reply")
	}
}

// /help must list every command the bot answers, and the containers it will
// actually restart -- built from the allowlist so it cannot claim more or less
// than sudo permits.
func TestHelpListsCommandsAndRestartables(t *testing.T) {
	b := &Bot{AllowedUserID: 42, Restartable: []string{"kavita", "plex"}}

	for _, text := range []string{"/help", "/start", "/nonsense"} {
		got := b.Handle(context.Background(), &Message{From: &User{ID: 42}, Text: text})
		for _, want := range []string{"/status", "/backup_now", "/restart", "/help", "kavita", "plex", "inbox"} {
			if !strings.Contains(got, want) {
				t.Errorf("help for %q omitted %q: %s", text, want, got)
			}
		}
	}
}

// The help text is sent with HTML parse mode, so an unescaped angle bracket
// would make Telegram drop the whole message.
func TestHelpEscapesContainerNames(t *testing.T) {
	b := &Bot{AllowedUserID: 42, Restartable: []string{"weird<&>name"}}
	got := b.help()
	if strings.Contains(got, "weird<&>name") {
		t.Errorf("container name was not escaped: %s", got)
	}
	if !strings.Contains(got, "weird&lt;&amp;&gt;name") {
		t.Errorf("container name not escaped as expected: %s", got)
	}
}
