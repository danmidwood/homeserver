package main

import (
	"context"
	"html"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Runner executes a command. Injected so that the privileged paths are
// testable without sudo, docker or systemd being present.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

type Bot struct {
	TG            *Client
	PromURL       string
	AlertURL      string
	AllowedUserID int64
	Restartable   []string
	InboxDir      string
	StateFile     string
	Run           Runner
}

// help lists what the bot can do, including which containers /restart accepts.
// The list is built from the configured allowlist rather than written out, so
// it cannot drift from what sudo will actually permit.
func (b *Bot) help() string {
	var sb strings.Builder
	sb.WriteString("<b>Commands</b>\n")
	sb.WriteString("/status — containers, disk, backup age, firing alerts\n")
	sb.WriteString("/backup_now — start a backup now\n")
	sb.WriteString("/restart &lt;service&gt; — restart one app container\n")
	sb.WriteString("/doorbell [today|yesterday|YYYY-MM-DD] [all] — doorbell clips\n")
	sb.WriteString("/whatspiked [HH:MM] — explain a load or temperature spike\n")
	sb.WriteString("/help — this message\n\n")
	sb.WriteString("Send a photo or a document to save it to the inbox (20 MB limit).\n\n")

	if len(b.Restartable) > 0 {
		sb.WriteString("<b>Restartable</b>\n")
		for _, c := range b.Restartable {
			sb.WriteString("• " + html.EscapeString(c) + "\n")
		}
		sb.WriteString("\nCaddy, the databases and the monitoring stack are deliberately excluded — restart those over SSH.")
	}
	return sb.String()
}

// parseCommand splits "/restart kavita" into "/restart" and "kavita". Telegram
// appends "@botname" to commands in groups, which is stripped here so the same
// text works in either context.
func parseCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	cmd, arg, _ := strings.Cut(text, " ")
	if at := strings.Index(cmd, "@"); at >= 0 {
		cmd = cmd[:at]
	}
	return cmd, strings.TrimSpace(arg)
}

// authorized fails closed. A message with no sender, a zero id, or an
// unconfigured AllowedUserID is rejected -- misconfiguration must never mean
// "allow everyone".
func (b *Bot) authorized(m *Message) bool {
	if m == nil || m.From == nil {
		return false
	}
	if b.AllowedUserID == 0 || m.From.ID == 0 {
		return false
	}
	return m.From.ID == b.AllowedUserID
}

// Handle returns the reply to send, or "" for no reply.
func (b *Bot) Handle(ctx context.Context, m *Message) string {
	if m.Document != nil || len(m.Photo) > 0 {
		return b.saveIncomingFile(ctx, m)
	}

	cmd, arg := parseCommand(m.Text)
	switch cmd {
	case "/status":
		return b.status(ctx)
	case "/backup_now":
		return b.backupNow(ctx)
	case "/restart":
		return b.restart(ctx, arg)
	case "/doorbell":
		return b.doorbell(ctx, m.Chat.ID, arg)
	case "/whatspiked":
		return b.whatspiked(ctx, arg)
	case "/help", "/start":
		return b.help()
	default:
		// An unrecognised command gets the same help rather than an error:
		// there is one user, and telling them what exists is more useful than
		// telling them what does not.
		return b.help()
	}
}

// loadOffset returns 0 for a missing or unreadable file. Starting from 0 means
// Telegram resends whatever it still holds, which is the safe direction: the
// alternative is skipping messages.
func loadOffset(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// saveOffset writes atomically: a torn write would be read back as 0 and
// replay every command Telegram still holds.
func saveOffset(path string, offset int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(offset)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Poll runs until the context is cancelled.
func (b *Bot) Poll(ctx context.Context) {
	if err := os.MkdirAll(filepath.Dir(b.StateFile), 0o700); err != nil {
		log.Printf("state directory: %v", err)
	}
	offset := loadOffset(b.StateFile)
	backoff := time.Second

	for ctx.Err() == nil {
		updates, err := b.TG.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Back off rather than hammering an unreachable Telegram or
			// crash-looping under systemd's Restart=always.
			log.Printf("getUpdates: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			if u.Callback != nil {
				b.handleCallback(ctx, u.Callback)
			}
			if u.Message != nil {
				if b.authorized(u.Message) {
					if reply := b.Handle(ctx, u.Message); reply != "" {
						if err := b.TG.SendMessage(ctx, u.Message.Chat.ID, reply); err != nil {
							log.Printf("sendMessage: %v", err)
						}
					}
				} else {
					// journald is the record here; there is no metric, and no
					// reply, because a reply confirms the bot exists.
					var id int64
					if u.Message.From != nil {
						id = u.Message.From.ID
					}
					log.Printf("ignoring message from unauthorised user id %d", id)
				}
			}
			// Advanced only after handling, so a crash replays at most the
			// message being processed rather than the whole backlog.
			offset = u.UpdateID + 1
			if err := saveOffset(b.StateFile, offset); err != nil {
				log.Printf("saving offset: %v", err)
			}
		}
	}
}

// handleCallback deals with an inline keyboard press.
//
// The same authorisation check as messages applies: a callback carries its own
// sender, and Telegram will happily deliver one from anybody who can see the
// message.
func (b *Bot) handleCallback(ctx context.Context, cb *CallbackQuery) {
	if cb.From == nil || b.AllowedUserID == 0 || cb.From.ID != b.AllowedUserID {
		var id int64
		if cb.From != nil {
			id = cb.From.ID
		}
		log.Printf("ignoring callback from unauthorised user id %d", id)
		return
	}

	// Answered first, and always. Telegram spins the button until this returns,
	// so answering only on success makes a slow upload look like a hang.
	if err := b.TG.AnswerCallback(ctx, cb.ID, ""); err != nil {
		log.Printf("answerCallbackQuery: %v", err)
	}

	var chatID int64
	if cb.Message != nil && cb.Message.Chat != nil {
		chatID = cb.Message.Chat.ID
	}
	if chatID == 0 {
		log.Printf("callback with no chat to reply to")
		return
	}

	var reply string
	switch {
	case strings.HasPrefix(cb.Data, "p:"):
		reply = b.playClip(ctx, chatID, strings.TrimPrefix(cb.Data, "p:"))
	default:
		reply = "Unrecognised button."
	}
	if reply != "" {
		if err := b.TG.SendMessage(ctx, chatID, reply); err != nil {
			log.Printf("sendMessage: %v", err)
		}
	}
}
