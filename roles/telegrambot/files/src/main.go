package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is not set", key)
	}
	return v
}

func main() {
	log.SetFlags(0) // journald already timestamps every line

	userID, err := strconv.ParseInt(mustEnv("TELEGRAM_ALLOWED_USER_ID"), 10, 64)
	if err != nil || userID == 0 {
		log.Fatalf("TELEGRAM_ALLOWED_USER_ID must be a non-zero integer")
	}

	inbox := mustEnv("TELEGRAM_INBOX_DIR")
	if err := os.MkdirAll(inbox, 0o750); err != nil {
		log.Fatalf("inbox directory: %v", err)
	}

	b := &Bot{
		TG: &Client{
			BaseURL: "https://api.telegram.org",
			Token:   mustEnv("TELEGRAM_BOT_TOKEN"),
			HTTP:    newHTTPClient(),
		},
		PromURL:       mustEnv("PROMETHEUS_URL"),
		AlertURL:      mustEnv("ALERTMANAGER_URL"),
		VikunjaURL:    os.Getenv("VIKUNJA_URL"),
		VikunjaToken:  os.Getenv("VIKUNJA_TOKEN"),
		AllowedUserID: userID,
		Restartable:   strings.Split(mustEnv("TELEGRAM_RESTARTABLE"), ","),
		InboxDir:      inbox,
		StateFile:     mustEnv("TELEGRAM_STATE_FILE"),
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The bot listens on nothing. Its liveness is reported by node-exporter's
	// systemd collector reading this unit's state, which needs no port and no
	// cooperation from the process being watched.
	log.Printf("telegram bot started")
	b.Poll(ctx)
	log.Printf("shutting down")
}
