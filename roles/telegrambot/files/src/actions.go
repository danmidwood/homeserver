package main

import (
	"context"
	"fmt"
	"html"
	"strings"
)

// restart bounces one allow-listed container.
//
// The name is compared against the list with an exact string match, never a
// prefix or a pattern. sudo enforces the same list independently -- this check
// exists so the reply can explain a refusal, not as the security boundary.
func (b *Bot) restart(ctx context.Context, name string) string {
	if name == "" {
		return "Usage: /restart &lt;service&gt;\nAllowed: " + html.EscapeString(strings.Join(b.Restartable, ", "))
	}
	allowed := false
	for _, c := range b.Restartable {
		if c == name {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Sprintf("%s is not restartable from here.\nAllowed: %s\n\nInfrastructure (caddy, the monitoring stack, the databases) is deliberately excluded — restart those over SSH.",
			html.EscapeString(name), html.EscapeString(strings.Join(b.Restartable, ", ")))
	}

	// sudo -n never prompts: with no tty a prompt would hang until the context
	// expires, and the reply would be a timeout rather than the real cause.
	out, err := b.Run(ctx, "sudo", "-n", "/usr/bin/docker", "restart", name)
	if err != nil {
		return fmt.Sprintf("❌ restarting %s failed: %s", html.EscapeString(name), html.EscapeString(trim(string(out), 300)))
	}
	return fmt.Sprintf("✅ restarted %s", html.EscapeString(name))
}

func (b *Bot) backupNow(ctx context.Context) string {
	out, _ := b.Run(ctx, "systemctl", "is-active", "restic-backup.service")
	if strings.TrimSpace(string(out)) == "active" {
		return "A backup is already running."
	}

	// --no-block is required: systemctl start blocks until a Type=oneshot unit
	// finishes, and restic-backup.service has a six-hour start timeout.
	//
	// This command line must match the sudoers grant token for token, or sudo
	// refuses it. A test asserts the exact string.
	out, err := b.Run(ctx, "sudo", "-n", "/usr/bin/systemctl", "start", "--no-block", "restic-backup.service")
	if err != nil {
		return "❌ could not start the backup: " + html.EscapeString(trim(string(out), 300))
	}
	// No completion message follows. The unit's OnFailure= already raises
	// BackupFailed, and a success updates the metric /status reads.
	return "✅ backup started. You will get an alert if it fails."
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
