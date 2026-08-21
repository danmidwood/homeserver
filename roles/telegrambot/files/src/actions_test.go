package main

import (
	"context"
	"strings"
	"testing"
)

func recordingRunner(out string, err error) (Runner, *[][]string) {
	var calls [][]string
	r := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte(out), err
	}
	return r, &calls
}

// The allowlist is the difference between bouncing an app and taking every
// site on the host offline, so it is tested name by name.
func TestRestartRefusesAnythingNotAllowed(t *testing.T) {
	run, calls := recordingRunner("", nil)
	b := &Bot{Restartable: []string{"kavita", "plex"}, Run: run}

	for _, name := range []string{"caddy", "prometheus", "alertmanager", "planka-postgres", "KAVITA", "kavita; rm -rf /", "../kavita", "kavita "} {
		reply := b.restart(context.Background(), name)
		if !strings.Contains(strings.ToLower(reply), "not restartable") {
			t.Errorf("restart(%q) did not refuse: %q", name, reply)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a refused restart still executed a command: %v", *calls)
	}
}

func TestRestartWithNoArgumentGivesUsage(t *testing.T) {
	run, calls := recordingRunner("", nil)
	b := &Bot{Restartable: []string{"kavita"}, Run: run}
	reply := b.restart(context.Background(), "")
	if !strings.Contains(strings.ToLower(reply), "usage") {
		t.Errorf("expected usage text, got %q", reply)
	}
	if len(*calls) != 0 {
		t.Errorf("an empty name executed a command: %v", *calls)
	}
}

func TestRestartRunsExactSudoCommand(t *testing.T) {
	run, calls := recordingRunner("kavita\n", nil)
	b := &Bot{Restartable: []string{"kavita"}, Run: run}

	b.restart(context.Background(), "kavita")

	if len(*calls) != 1 {
		t.Fatalf("expected one command, got %v", *calls)
	}
	got := strings.Join((*calls)[0], " ")
	want := "sudo -n /usr/bin/docker restart kavita"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// systemctl start on a Type=oneshot unit blocks until the unit finishes, and
// the backup unit has a six-hour timeout. Without --no-block the bot hangs for
// the length of a backup.
func TestBackupNowUsesNoBlock(t *testing.T) {
	run, calls := recordingRunner("inactive\n", nil)
	b := &Bot{Run: run}

	b.backupNow(context.Background())

	joined := ""
	for _, c := range *calls {
		joined += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(joined, "--no-block") {
		t.Errorf("backup_now did not use --no-block: %q", joined)
	}
	if !strings.Contains(joined, "restic-backup.service") {
		t.Errorf("backup_now did not start the backup unit: %q", joined)
	}
}

// The sudoers grant is written as an exact command line. If this string and
// the template ever diverge by a single token, sudo refuses at runtime.
func TestBackupNowMatchesTheSudoersGrantExactly(t *testing.T) {
	run, calls := recordingRunner("inactive\n", nil)
	b := &Bot{Run: run}
	b.backupNow(context.Background())

	var started string
	for _, c := range *calls {
		if strings.Contains(strings.Join(c, " "), "start") {
			started = strings.Join(c, " ")
		}
	}
	want := "sudo -n /usr/bin/systemctl start --no-block restic-backup.service"
	if started != want {
		t.Errorf("got %q, want %q", started, want)
	}
}

func TestBackupNowReportsAlreadyRunning(t *testing.T) {
	run, calls := recordingRunner("active\n", nil)
	b := &Bot{Run: run}

	reply := b.backupNow(context.Background())

	if !strings.Contains(strings.ToLower(reply), "already") {
		t.Errorf("did not report an in-flight backup: %q", reply)
	}
	for _, c := range *calls {
		if strings.Contains(strings.Join(c, " "), "start") {
			t.Errorf("started a backup while one was already running: %v", *calls)
		}
	}
}
