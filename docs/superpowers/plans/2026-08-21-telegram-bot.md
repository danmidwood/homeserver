# Inbound Telegram Bot Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go service that lets one Telegram user ask the server for its status, trigger a backup, restart an allow-listed app container, and drop files into an inbox.

**Architecture:** A single stdlib-only Go binary run by systemd as an unprivileged `telegrambot` user. It long-polls Telegram, reads `/status` from Prometheus and Alertmanager over HTTP, and performs its two privileged actions through exact-match sudo rules — no wildcards, and the bot user is never in the `docker` group. It exposes no network listener at all; its liveness is monitored by node-exporter's systemd collector reading the unit's state.

**Tech Stack:** Go 1.23 (standard library only, zero third-party dependencies), systemd, sudo, Ansible, Prometheus.

**Spec:** `docs/superpowers/specs/2026-08-21-telegram-bot-design.md`

## Global Constraints

- **Go standard library only.** No third-party dependencies, so no `go.sum` supply chain and no network at build time. The build sets `GOPROXY=off` to prove it.
- **The bot user is never in the `docker` group.** Docker socket access is equivalent to root.
- **No wildcards in sudoers.** One exact-match line per permitted action. sudo is the allowlist; the bot's own validation is defence in depth, not the boundary.
- **Never set `NoNewPrivileges=true`** on the unit — it silently breaks sudo, and the failure looks like a permissions bug.
- **Every sudoers file is installed with `validate: 'visudo -cf %s'`.** A malformed sudoers file breaks sudo for every user on the host.
- Telegram receivers use `parse_mode: HTML`, which drops the entire message on a stray `<`, `>` or `&`. Every interpolated value is escaped with `html.EscapeString`.
- Alertmanager routes only on severity `critical`, `warning`, `info`, `none`.
- Secrets live only in gitignored files. No real token, chat id, or credential may appear in any committed file, including this plan and the tests.
- Ansible role tasks must be idempotent: a second run reports `changed=0`.
- No code comment anywhere may mention `/mnt/seagate`.

## Facts Already Verified — Do Not Re-Derive

| Fact | Value |
|---|---|
| Go on the host | `go1.23.4 linux/amd64`, installed as the pacman package `go` |
| Host arch | `x86_64` |
| `sudo` env handling | `env_reset` is the compiled-in default and is **not** overridden; the only active `env_keep` is scoped to `visudo`. So `DOCKER_HOST` cannot pass through, and no wrapper script is needed. |
| Docker group members | `daniel` only |
| Existing sudoers drop-ins | none |
| node-exporter systemd collector | Available (`--collector.systemd`, with `--collector.systemd.unit-include`), currently **not** enabled |
| Current node-exporter args | `NODE_EXPORTER_ARGS="--collector.textfile.directory=/var/lib/node_exporter/textfile_collector"` in `/etc/conf.d/prometheus-node-exporter` |
| Phase 1 secrets already present | `telegram_bot_token`, `telegram_chat_id` in `user_passwords.yml`. For a private chat the chat id **is** the user id. |
| Textfile collector dir | `/var/lib/node_exporter/textfile_collector`, `root:root 0755` — the bot deliberately does **not** write here |
| Prometheus API | `http://localhost:9090`, Alertmanager `http://localhost:9093` |
| `/mnt/storage` free | 261G |

**Deviation from the spec, deliberate.** The spec calls for a heartbeat written to the node-exporter textfile collector plus a new `TelegramBotDown` rule. This plan enables node-exporter's systemd collector instead and alerts on unit state.

Writing to the textfile directory is rejected because that directory also holds `restic_backup.prom` and `smart.prom`; granting this service write access there would let a compromised bot forge the backup and disk-health metrics — hiding exactly the failures the alerting exists to catch.

Having the bot serve `/metrics` for Prometheus to scrape was also rejected. It would mean a new listening socket on all interfaces, since Prometheus is containerised and reaches the host over the docker bridge rather than loopback, and it would make liveness depend on the service answering for itself — a wedged bot still accepting TCP would pass a scrape while being useless.

Reading systemd's own view of the unit needs no port, no metrics code in the bot, and no cooperation from the thing being monitored. It also closes a gap the phase 1 spec already records: *"`backup-alert.service` has no `OnFailure=` of its own, so if the handler itself dies nothing notifies. A general fix needs node-exporter's `systemd` collector, which is not enabled."* One collector covers `telegram-bot`, `restic-backup`, `backup-alert`, `image-update-check` and `docker`. Task 6 updates the spec to match.

## File Structure

| File | Responsibility |
|---|---|
| `roles/telegrambot/files/src/go.mod` | Module declaration, Go 1.23, zero requires |
| `roles/telegrambot/files/src/telegram.go` | Telegram Bot API client: types, getUpdates, sendMessage, getFile, download |
| `roles/telegrambot/files/src/bot.go` | Bot struct, poll loop, offset persistence, authorisation, dispatch |
| `roles/telegrambot/files/src/status.go` | Prometheus and Alertmanager queries, `/status` rendering |
| `roles/telegrambot/files/src/actions.go` | `/backup_now` and `/restart`, via an injectable command runner |
| `roles/telegrambot/files/src/inbox.go` | Filename sanitising, collision handling, file save |
| `roles/telegrambot/files/src/main.go` | Configuration from environment, wiring, startup |
| `roles/telegrambot/files/src/*_test.go` | Tests, offline, no network and no real bot |
| `roles/telegrambot/defaults/main.yml` | `telegrambot_restartable` — the nine allowed containers |
| `roles/telegrambot/templates/sudoers.j2` | Generated exact-match rules |
| `roles/telegrambot/templates/env.j2` | Token, allowed user id, paths |
| `roles/telegrambot/files/telegram-bot.service` | systemd unit |
| `roles/telegrambot/tasks/main.yml` | User, directories, source, build, sudoers, unit |
| `roles/telegrambot/handlers/main.yml` | Restart handler |
| `tests/run-go-tests.sh` | Runs `go test ./...` for the bot |
| `roles/prometheus/tasks/main.yml` | Enable the systemd collector on node-exporter |
| `roles/prometheus/files/rules/systemd.yml` | `SystemdUnitFailed` rule |
| `tests/rules/systemd_test.yml` | Its promtool unit test |
| `roles/backup/defaults/main.yml` | Inbox added to `backup_paths` |
| `playbooks/xps.yml` | One added role line |

---

### Task 1: Telegram client, poll loop, authorisation and dispatch

**Files:**
- Create: `roles/telegrambot/files/src/go.mod`, `telegram.go`, `bot.go`, `telegram_test.go`, `bot_test.go`
- Create: `tests/run-go-tests.sh`

**Interfaces:**
- Produces, relied on by every later task:
  - `type Client struct { BaseURL, Token string; HTTP *http.Client }`
  - `func (c *Client) GetUpdates(ctx context.Context, offset, timeoutSec int) ([]Update, error)`
  - `func (c *Client) SendMessage(ctx context.Context, chatID int64, html string) error`
  - `func (c *Client) GetFile(ctx context.Context, fileID string) (string, error)`
  - `func (c *Client) Download(ctx context.Context, filePath string, w io.Writer) error`
  - `type Bot struct` with fields `TG *Client`, `PromURL, AlertURL string`, `AllowedUserID int64`, `Restartable []string`, `InboxDir, StateFile string`, `Run Runner`
  - `type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)`
  - `func (b *Bot) Handle(ctx context.Context, m *Message) string`
  - `func parseCommand(text string) (cmd, arg string)`
  - `func loadOffset(path string) int` / `func saveOffset(path string, offset int) error`

- [ ] **Step 1: Write the failing tests**

Create `roles/telegrambot/files/src/bot_test.go`:

```go
package main

import (
	"context"
	"os"
	"path/filepath"
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
```

Create `roles/telegrambot/files/src/telegram_test.go`:

```go
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
		if r.URL.Query().Get("offset") != "7" {
			t.Errorf("offset not passed through: %q", r.URL.Query().Get("offset"))
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd roles/telegrambot/files/src && go test ./...`
Expected: FAIL — the package does not compile, because none of these types or functions exist yet.

- [ ] **Step 3: Write go.mod**

```
module homeserver/telegrambot

go 1.23
```

There is no `require` block and there must never be one: the standard library covers all of this, and adding a dependency reintroduces a supply chain to audit.

- [ ] **Step 4: Write telegram.go**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a minimal Telegram Bot API client. Four commands do not justify a
// framework, and the standard library keeps the dependency count at zero.
type Client struct {
	BaseURL string // https://api.telegram.org, overridden in tests
	Token   string
	HTTP    *http.Client
}

type User struct {
	ID int64 `json:"id"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	FileSize     int64  `json:"file_size"`
}

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
}

type Message struct {
	MessageID int         `json:"message_id"`
	From      *User       `json:"from"`
	Chat      *Chat       `json:"chat"`
	Text      string      `json:"text"`
	Caption   string      `json:"caption"`
	Document  *Document   `json:"document"`
	Photo     []PhotoSize `json:"photo"`
}

type Update struct {
	UpdateID int      `json:"update_id"`
	Message  *Message `json:"message"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func (c *Client) call(ctx context.Context, method string, form url.Values) (json.RawMessage, error) {
	endpoint := fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(c.BaseURL, "/"), c.Token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var ar apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, fmt.Errorf("%s: decoding response: %w", method, err)
	}
	// A non-2xx status or ok:false must surface as an error. Treating either as
	// success would make a dropped message look like a delivered one.
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !ar.OK {
		return nil, fmt.Errorf("%s: telegram returned %d: %s", method, resp.StatusCode, ar.Description)
	}
	return ar.Result, nil
}

func (c *Client) GetUpdates(ctx context.Context, offset, timeoutSec int) ([]Update, error) {
	form := url.Values{}
	form.Set("offset", strconv.Itoa(offset))
	form.Set("timeout", strconv.Itoa(timeoutSec))
	// Only message updates are of interest; asking for less means Telegram sends
	// less.
	form.Set("allowed_updates", `["message"]`)

	raw, err := c.call(ctx, "getUpdates", form)
	if err != nil {
		return nil, err
	}
	var ups []Update
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, fmt.Errorf("getUpdates: decoding result: %w", err)
	}
	return ups, nil
}

// SendMessage sends text already escaped by the caller. It must not escape
// again: HTML parse mode means double-escaping turns &amp; into &amp;amp; and
// corrupts every message that contains one.
func (c *Client) SendMessage(ctx context.Context, chatID int64, html string) error {
	form := url.Values{}
	form.Set("chat_id", strconv.FormatInt(chatID, 10))
	form.Set("text", html)
	form.Set("parse_mode", "HTML")
	form.Set("disable_web_page_preview", "true")
	_, err := c.call(ctx, "sendMessage", form)
	return err
}

// GetFile resolves a file_id to the path used by the download endpoint.
func (c *Client) GetFile(ctx context.Context, fileID string) (string, error) {
	form := url.Values{}
	form.Set("file_id", fileID)
	raw, err := c.call(ctx, "getFile", form)
	if err != nil {
		return "", err
	}
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", err
	}
	if f.FilePath == "" {
		return "", fmt.Errorf("getFile: telegram returned no file_path")
	}
	return f.FilePath, nil
}

// Download streams a file. Downloads use a different URL shape from API calls:
// /file/bot<token>/<path> rather than /bot<token>/<method>.
func (c *Client) Download(ctx context.Context, filePath string, w io.Writer) error {
	endpoint := fmt.Sprintf("%s/file/bot%s/%s", strings.TrimRight(c.BaseURL, "/"), c.Token, filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("download: telegram returned %d", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 90 * time.Second}
}
```

- [ ] **Step 5: Write bot.go**

```go
package main

import (
	"context"
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

const helpText = `Commands:
/status - containers, disk, backup, alerts
/backup_now - start a backup now
/restart &lt;service&gt; - restart one app container
Send a file to save it to the inbox.`

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
	case "/help", "/start":
		return helpText
	default:
		return helpText
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
			if u.Message != nil {
				if b.authorized(u.Message) {
					if reply := b.Handle(ctx, u.Message); reply != "" {
						if err := b.TG.SendMessage(ctx, u.Message.Chat.ID, reply); err != nil {
							log.Printf("sendMessage: %v", err)
						}
					}
				} else {
					// Logged, never answered: a reply confirms the bot exists
					// to whoever is probing.
					var id int64
					if u.Message.From != nil {
						id = u.Message.From.ID
					}
					// journald is the record here; there is no metric, and no
					// reply, because a reply confirms the bot exists.
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

```

- [ ] **Step 6: Write the test harness**

Create `tests/run-go-tests.sh`:

```bash
#!/usr/bin/env bash
# Runs the Telegram bot's Go tests. Offline: every test uses httptest, so no
# network, no real bot token, and no server access are needed.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO_ROOT/roles/telegrambot/files/src"

echo "==> go vet"
(cd "$SRC" && GOPROXY=off go vet ./...)

echo "==> go test"
(cd "$SRC" && GOPROXY=off go test ./...)

echo "==> confirming there are no third-party dependencies"
if grep -q "^require" "$SRC/go.mod"; then
  echo "  FAIL: go.mod has a require block; this project is stdlib-only"
  exit 1
fi
if [ -f "$SRC/go.sum" ]; then
  echo "  FAIL: go.sum exists; this project is stdlib-only"
  exit 1
fi
echo "  OK: stdlib only"
```

Make it executable with `chmod +x tests/run-go-tests.sh`.

- [ ] **Step 7: Make it compile**

`bot.go` above references `b.status`, `b.backupNow`, `b.restart` and `b.saveIncomingFile`, which arrive in Tasks 2 to 4. To keep this task independently testable, add a temporary file `roles/telegrambot/files/src/stubs.go` containing exactly:

```go
package main

import "context"

// Replaced in later tasks. Present so Task 1 compiles and its tests run.

func (b *Bot) status(ctx context.Context) string            { return "not implemented" }
func (b *Bot) backupNow(ctx context.Context) string         { return "not implemented" }
func (b *Bot) restart(ctx context.Context, n string) string { return "not implemented" }

func (b *Bot) saveIncomingFile(ctx context.Context, m *Message) string { return "not implemented" }
```

Task 2 replaces `status`, Task 3 replaces `backupNow` and `restart`, and Task 4 replaces `saveIncomingFile`, each deleting the corresponding stub. **`stubs.go` must not exist by the end of Task 4.** Task 4 has an explicit step to verify that.

- [ ] **Step 8: Run tests to verify they pass**

Run: `./tests/run-go-tests.sh`
Expected: PASS — `go vet` clean, all tests pass, stdlib-only check passes.

- [ ] **Step 9: Mutation-verify the authorisation test**

The project requires proof a test can fail. Temporarily change `authorized` to `return true`, run `./tests/run-go-tests.sh`, and confirm `TestAuthorized` and `TestAuthorizedFailsClosedWhenUnset` both FAIL. Restore it and confirm they pass. Record both outcomes in your report.

- [ ] **Step 10: Commit**

```bash
git add roles/telegrambot tests/run-go-tests.sh
git commit -m "Add the Telegram bot's client, poll loop and authorisation

A stdlib-only Go service. The whole API surface is tested against httptest, so
the suite needs no network and no real bot.

Two behaviours carry the risk and so carry the tests. Authorisation fails
closed: no sender, a zero id, or an unset allowed id are all rejected, because
a misconfigured bot must not accept everyone. And the getUpdates offset is
persisted atomically and advanced only after a message is handled -- Telegram
redelivers unacknowledged updates for 24 hours, so without this a crash loop
would replay the commands in them."
```

---

### Task 2: `/status` from Prometheus and Alertmanager

**Files:**
- Create: `roles/telegrambot/files/src/status.go`, `status_test.go`
- Modify: `roles/telegrambot/files/src/stubs.go` — delete the `status` stub

**Interfaces:**
- Consumes: `Bot` from Task 1.
- Produces: `func (b *Bot) status(ctx context.Context) string`, and `func promScalar(ctx context.Context, base, query string) (float64, error)`.

- [ ] **Step 1: Write the failing test**

Create `roles/telegrambot/files/src/status_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `./tests/run-go-tests.sh`
Expected: FAIL — `promScalar` is undefined, and `status` returns `"not implemented"`.

- [ ] **Step 3: Write status.go**

```go
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

func firingAlertNames(ctx context.Context, base string) ([]string, error) {
	endpoint := strings.TrimRight(base, "/") + "/api/v2/alerts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var alerts []struct {
		Labels map[string]string `json:"labels"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		return nil, err
	}

	counts := map[string]int{}
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
	return out, nil
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

	if names, err := firingAlertNames(ctx, b.AlertURL); err != nil {
		lines = append(lines, "❓ alerts: Alertmanager did not answer")
	} else if len(names) == 0 {
		lines = append(lines, "✅ no alerts firing")
	} else {
		lines = append(lines, fmt.Sprintf("⚠️ %d firing: %s", len(names), strings.Join(names, ", ")))
	}

	return strings.Join(lines, "\n")
}
```

- [ ] **Step 4: Delete the status stub**

Remove the `func (b *Bot) status(...)` line from `stubs.go`.

- [ ] **Step 5: Run tests to verify they pass**

Run: `./tests/run-go-tests.sh`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add roles/telegrambot/files/src
git commit -m "Add /status, read from Prometheus and Alertmanager

The bot consumes the monitoring rather than inspecting the host, so /status
needs no privilege at all and never touches the Docker socket.

An empty Prometheus result is an error rather than zero: an absent metric
rendered as 0 would turn 'cAdvisor is gone' into '0 containers running', which
is a confident wrong answer instead of an honest failure. Alert names are
HTML-escaped because Telegram drops any message containing a stray angle
bracket -- an unescaped name means no status message at all."
```

---

### Task 3: `/backup_now` and `/restart`

**Files:**
- Create: `roles/telegrambot/files/src/actions.go`, `actions_test.go`
- Modify: `roles/telegrambot/files/src/stubs.go` — delete the `backupNow` and `restart` stubs

**Interfaces:**
- Consumes: `Bot` and `Runner` from Task 1.
- Produces: `func (b *Bot) backupNow(ctx context.Context) string`, `func (b *Bot) restart(ctx context.Context, name string) string`.

- [ ] **Step 1: Write the failing test**

Create `roles/telegrambot/files/src/actions_test.go`:

```go
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

	for _, name := range []string{"caddy", "prometheus", "alertmanager", "planka-postgres", "", "KAVITA", "kavita; rm -rf /", "../kavita"} {
		reply := b.restart(context.Background(), name)
		if !strings.Contains(strings.ToLower(reply), "not") {
			t.Errorf("restart(%q) did not refuse: %q", name, reply)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a refused restart still executed a command: %v", *calls)
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `./tests/run-go-tests.sh`
Expected: FAIL — the stubs return `"not implemented"` and run no commands.

- [ ] **Step 3: Write actions.go**

```go
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
```

- [ ] **Step 4: Delete the stubs**

Remove the `backupNow` and `restart` lines from `stubs.go`.

- [ ] **Step 5: Run tests to verify they pass**

Run: `./tests/run-go-tests.sh`
Expected: PASS.

- [ ] **Step 6: Mutation-verify the allowlist test**

Change the allowlist loop in `restart` to `allowed = true` unconditionally, run `./tests/run-go-tests.sh`, and confirm `TestRestartRefusesAnythingNotAllowed` FAILS. Restore and confirm it passes. Record both outcomes in your report.

- [ ] **Step 7: Commit**

```bash
git add roles/telegrambot/files/src
git commit -m "Add /backup_now and /restart

Both go through sudo with an exact command line and no wildcards. The bot's own
allowlist check is defence in depth so a refusal can be explained; sudo enforces
the same list independently and is the actual boundary.

sudo -n never prompts, because with no tty a prompt would hang until the context
expired and report a timeout instead of the real cause. /backup_now uses
--no-block, without which systemctl start would block for the length of a
backup, and reports an already-running backup rather than silently doing
nothing."
```

---

### Task 4: The file inbox

**Files:**
- Create: `roles/telegrambot/files/src/inbox.go`, `inbox_test.go`
- Modify: `roles/telegrambot/files/src/stubs.go` — delete the `saveIncomingFile` stub

**Interfaces:**
- Consumes: `Client.GetFile`, `Client.Download` from Task 1.
- Produces: `func sanitizeFilename(name string) string`, `func uniquePath(dir, name string) (string, error)`, `func (b *Bot) saveIncomingFile(ctx context.Context, m *Message) string`.

- [ ] **Step 1: Write the failing test**

Create `roles/telegrambot/files/src/inbox_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Filenames arrive from a remote sender. Every case here is an attempt to
// write outside the inbox or to create something awkward to delete.
func TestSanitizeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"receipt.pdf", "receipt.pdf"},
		{"../../etc/passwd", "passwd"},
		{"/etc/shadow", "shadow"},
		{`..\..\windows\system32`, "system32"},
		{".hidden", "hidden"},
		{"..", "file"},
		{".", "file"},
		{"", "file"},
		{"a b c.txt", "a_b_c.txt"},
		{"emoji🎉.png", "emoji_.png"},
		{"semi;colon&amp.txt", "semi_colon_amp.txt"},
		{strings.Repeat("x", 300) + ".txt", strings.Repeat("x", 120)},
	}
	for _, c := range cases {
		got := sanitizeFilename(c.in)
		if got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// No sanitised name may contain a path separator or escape its directory.
func TestSanitizedNamesNeverEscapeTheInbox(t *testing.T) {
	dir := t.TempDir()
	for _, in := range []string{"../../etc/passwd", "/etc/shadow", "..", "....//....//x"} {
		full := filepath.Join(dir, sanitizeFilename(in))
		if !strings.HasPrefix(filepath.Clean(full), filepath.Clean(dir)+string(os.PathSeparator)) {
			t.Errorf("input %q produced a path outside the inbox: %q", in, full)
		}
	}
}

// An existing file must never be silently overwritten.
func TestUniquePathAvoidsCollisions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := uniquePath(dir, "a.txt")
	if err != nil {
		t.Fatalf("uniquePath: %v", err)
	}
	if filepath.Base(p) == "a.txt" {
		t.Error("uniquePath returned a path that would overwrite an existing file")
	}
	if !strings.HasPrefix(filepath.Base(p), "a") || !strings.HasSuffix(filepath.Base(p), ".txt") {
		t.Errorf("uniquePath lost the name or extension: %q", filepath.Base(p))
	}

	// The original must still hold its content.
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(data) != "first" {
		t.Error("the existing file was modified")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `./tests/run-go-tests.sh`
Expected: FAIL — `sanitizeFilename` and `uniquePath` are undefined.

- [ ] **Step 3: Write inbox.go**

```go
package main

import (
	"context"
	"fmt"
	"html"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Telegram's Bot API will not serve files larger than this. Raising it needs a
// local Bot API server, which is not worth the infrastructure.
const maxFileBytes = 20 * 1024 * 1024

const maxNameLen = 120

// sanitizeFilename turns a remote-supplied name into something safe to join to
// the inbox path.
//
// Order matters. The basename is taken first, using both separators, because a
// Windows client can send backslashes that Go's path handling would treat as an
// ordinary character. Only then is the character set reduced, so that a name
// like "../x" cannot survive by having its slashes rewritten into something
// harmless-looking after the fact.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)

	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name = b.String()

	// Leading dots would create hidden files, and "." and ".." are directory
	// entries rather than names.
	name = strings.TrimLeft(name, ".")

	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	if name == "" {
		name = "file"
	}
	return name
}

// uniquePath returns a path in dir that does not already exist, so an incoming
// file never overwrites one already saved.
func uniquePath(dir, name string) (string, error) {
	full := filepath.Join(dir, name)
	if _, err := os.Stat(full); os.IsNotExist(err) {
		return full, nil
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("could not find a free filename for %q", name)
}

func (b *Bot) saveIncomingFile(ctx context.Context, m *Message) string {
	var fileID, name string
	var size int64

	switch {
	case m.Document != nil:
		fileID, size, name = m.Document.FileID, m.Document.FileSize, m.Document.FileName
	case len(m.Photo) > 0:
		// Photos arrive as several sizes, largest last, and carry no filename.
		p := m.Photo[len(m.Photo)-1]
		fileID, size = p.FileID, p.FileSize
		name = fmt.Sprintf("photo-%s-%s.jpg", time.Now().UTC().Format("20060102-150405"), p.FileUniqueID)
	default:
		return ""
	}

	// Checked before calling getFile, so an oversized file is refused clearly
	// rather than failing part-way through a download.
	if size > maxFileBytes {
		return fmt.Sprintf("❌ that file is %.1f MB. Telegram's bot API will not serve anything over 20 MB.", float64(size)/(1024*1024))
	}

	safe := sanitizeFilename(name)
	dest, err := uniquePath(b.InboxDir, safe)
	if err != nil {
		return "❌ " + html.EscapeString(err.Error())
	}

	remotePath, err := b.TG.GetFile(ctx, fileID)
	if err != nil {
		return "❌ could not fetch that file: " + html.EscapeString(err.Error())
	}

	// Written to a temporary name and renamed, so a failed transfer never
	// leaves a truncated file looking like a complete one.
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "❌ could not create the file: " + html.EscapeString(err.Error())
	}
	if err := b.TG.Download(ctx, remotePath, f); err != nil {
		f.Close()
		os.Remove(tmp)
		return "❌ download failed: " + html.EscapeString(err.Error())
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "❌ could not finish writing: " + html.EscapeString(err.Error())
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "❌ could not save: " + html.EscapeString(err.Error())
	}

	return fmt.Sprintf("📎 saved as %s", html.EscapeString(filepath.Base(dest)))
}
```

- [ ] **Step 4: Delete stubs.go entirely and verify it is gone**

`saveIncomingFile` was the last stub, so the file goes rather than being emptied:

```bash
rm roles/telegrambot/files/src/stubs.go
test ! -f roles/telegrambot/files/src/stubs.go && echo "stubs.go removed"
```

If anything then fails to compile, a stub was still in use — implement it rather than restoring the file.

- [ ] **Step 5: Run tests to verify they pass**

Run: `./tests/run-go-tests.sh`
Expected: PASS.

- [ ] **Step 6: Mutation-verify the sanitiser**

Change `sanitizeFilename` to `return name` unchanged, run the tests, and confirm both `TestSanitizeFilename` and `TestSanitizedNamesNeverEscapeTheInbox` FAIL. Restore and confirm they pass. Record both outcomes in your report.

- [ ] **Step 7: Commit**

```bash
git add roles/telegrambot/files/src
git commit -m "Add the file inbox

Filenames arrive from a remote sender and are treated as hostile. The basename
is taken first, handling backslashes as separators because a Windows client can
send them, and only then is the character set reduced -- doing it the other way
round would let '../x' survive by having its slashes rewritten after the fact.
A test asserts that no input can produce a path outside the inbox.

The 20MB cap is checked before calling getFile, so an oversized file is refused
clearly instead of failing part-way through. Downloads are written to a .part
file and renamed, so a failed transfer never leaves a truncated file looking
complete, and collisions are suffixed rather than overwriting."
```

---

### Task 5: main, and the Ansible role

**Files:**
- Create: `roles/telegrambot/files/src/main.go`
- Create: `roles/telegrambot/defaults/main.yml`, `tasks/main.yml`, `handlers/main.yml`, `templates/sudoers.j2`, `templates/env.j2`, `files/telegram-bot.service`
- Modify: `playbooks/xps.yml` (insert exactly one line)

**Interfaces:**
- Consumes: everything from Tasks 1 to 4.
- Produces: a deployed, running service.

- [ ] **Step 1: Write main.go**

```go
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
```

- [ ] **Step 2: Write the role defaults**

`roles/telegrambot/defaults/main.yml`:

```yaml
# Containers the bot may restart. Application services only.
#
# Infrastructure is deliberately absent and must stay absent:
#   caddy                        fronts all seven sites; restarting it takes
#                                every one down at once
#   prometheus, alertmanager     restarting these blinds the monitoring, and
#                                alertmanager is what delivers the alert saying
#                                something is wrong. The bot must not be able to
#                                sever its own reporting path.
#   cadvisor, blackbox, grafana  monitoring, same argument
#   *-postgres, immich-redis     data stores; bouncing these under load is a
#                                different risk class from bouncing an app
#
# This list generates the sudoers file, so adding a name here grants real
# privilege. Restart anything not listed here over SSH.
telegrambot_restartable:
  - actual_budget
  - ftp_server
  - immich-machine-learning
  - immich-server
  - kavita
  - planka
  - plex
  - portainer
  - vaultwarden

telegrambot_inbox_dir: /mnt/storage/telegram-inbox
```

- [ ] **Step 3: Write the sudoers template**

`roles/telegrambot/templates/sudoers.j2`:

```
# Managed by Ansible. Do not edit on the host.
#
# The complete privileged capability of the telegram bot. Every line is an
# exact command with no wildcards, so sudo itself is the allowlist: a bug in
# the bot cannot widen what it is able to do.
#
# Adding a wildcard here would defeat the entire privilege model.
telegrambot ALL=(root) NOPASSWD: /usr/bin/systemctl start --no-block restic-backup.service
{% for name in telegrambot_restartable %}
telegrambot ALL=(root) NOPASSWD: /usr/bin/docker restart {{ name }}
{% endfor %}
```

- [ ] **Step 4: Write the env template**

`roles/telegrambot/templates/env.j2`:

```
TELEGRAM_BOT_TOKEN={{ telegram_bot_token }}
TELEGRAM_ALLOWED_USER_ID={{ telegram_chat_id }}
TELEGRAM_RESTARTABLE={{ telegrambot_restartable | join(',') }}
TELEGRAM_INBOX_DIR={{ telegrambot_inbox_dir }}
TELEGRAM_STATE_FILE=/var/lib/telegram-bot/offset
PROMETHEUS_URL=http://localhost:9090
ALERTMANAGER_URL=http://localhost:9093
```

- [ ] **Step 5: Write the systemd unit**

`roles/telegrambot/files/telegram-bot.service`:

```
[Unit]
Description=Inbound Telegram bot
Wants=network-online.target
After=network-online.target docker.service

[Service]
Type=simple
User=telegrambot
Group=telegrambot
EnvironmentFile=/etc/telegram-bot/env
ExecStart=/usr/local/bin/telegram-bot
Restart=always
RestartSec=5s

# NoNewPrivileges is deliberately NOT set. It blocks setuid binaries, which
# includes sudo, so setting it would break /backup_now and /restart with a
# failure that reads like a permissions bug rather than a unit misconfiguration.

PrivateTmp=true
ProtectHome=true
ProtectSystem=full
ReadWritePaths=/mnt/storage/telegram-inbox /var/lib/telegram-bot

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 6: Write the handler**

`roles/telegrambot/handlers/main.yml`:

```yaml
- name: Restart telegram bot
  ansible.builtin.systemd:
    name: telegram-bot
    state: restarted
    daemon_reload: true
```

- [ ] **Step 7: Write the role tasks**

`roles/telegrambot/tasks/main.yml`:

```yaml
- name: Ensure the Telegram credentials are configured
  ansible.builtin.assert:
    that:
      - telegram_bot_token | default('') | length > 0
      - telegram_chat_id | default('') | length > 0
    fail_msg: "telegram_bot_token and telegram_chat_id must be set in user_passwords.yml"

- name: Ensure the Go toolchain is installed
  # The bot is built from source on the host, so the repository stays the
  # single source of truth and no binary is committed.
  ansible.builtin.package:
    name: go
    state: present

- name: Create the telegrambot system user
  # No shell and no home. Deliberately NOT in the docker group: socket access
  # is equivalent to root, and this service takes instructions from the
  # internet.
  ansible.builtin.user:
    name: telegrambot
    system: true
    shell: /usr/sbin/nologin
    create_home: false
    home: /var/lib/telegram-bot

- name: Create the bot state directory
  ansible.builtin.file:
    path: /var/lib/telegram-bot
    state: directory
    owner: telegrambot
    group: telegrambot
    mode: '0700'

- name: Create the inbox directory
  ansible.builtin.file:
    path: "{{ telegrambot_inbox_dir }}"
    state: directory
    owner: telegrambot
    group: telegrambot
    mode: '0750'

- name: Create the configuration directory
  ansible.builtin.file:
    path: /etc/telegram-bot
    state: directory
    owner: root
    group: telegrambot
    mode: '0750'

- name: Write the bot environment file
  ansible.builtin.template:
    src: env.j2
    dest: /etc/telegram-bot/env
    owner: root
    group: telegrambot
    mode: '0640'
  # The rendered file contains the bot token. Without this it appears in
  # --diff and -v output.
  no_log: true
  notify: Restart telegram bot

- name: Install the sudoers rules
  ansible.builtin.template:
    src: sudoers.j2
    dest: /etc/sudoers.d/telegram-bot
    owner: root
    group: root
    mode: '0440'
    # Without validate, a malformed file here breaks sudo for every user on
    # the host, including the one needed to fix it.
    validate: 'visudo -cf %s'

- name: Copy the bot source
  ansible.builtin.copy:
    src: src/
    dest: /usr/local/src/telegram-bot/
    owner: root
    group: root
    mode: '0644'
  register: telegrambot_source

- name: Check whether the binary is already built
  ansible.builtin.stat:
    path: /usr/local/bin/telegram-bot
  register: telegrambot_binary

- name: Build the bot
  ansible.builtin.command:
    cmd: go build -trimpath -o /usr/local/bin/telegram-bot .
    chdir: /usr/local/src/telegram-bot
  environment:
    HOME: /root
    # The project is stdlib-only, so the build needs no network. Setting this
    # turns an accidentally added dependency into a build failure rather than
    # a silent download.
    GOPROXY: "off"
  when: telegrambot_source.changed or not telegrambot_binary.stat.exists
  changed_when: true
  notify: Restart telegram bot

- name: Install the systemd unit
  ansible.builtin.copy:
    src: telegram-bot.service
    dest: /etc/systemd/system/telegram-bot.service
    owner: root
    group: root
    mode: '0644'
  notify: Restart telegram bot

- name: Enable and start the bot
  ansible.builtin.systemd:
    name: telegram-bot
    enabled: true
    state: started
    daemon_reload: true
```

- [ ] **Step 8: Add the role to the playbook**

Open `playbooks/xps.yml`. Count the lines matching `^    - ` and write the number down. Insert **exactly one line**, `    - telegrambot`, immediately after the `    - imagewatch` line. Count again and confirm the total rose by exactly one and no other role name changed.

- [ ] **Step 9: Deploy**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
```

Then verify:

```bash
ssh daniel@xps.fritz.box 'systemctl is-active telegram-bot; journalctl -u telegram-bot -n 20 --no-pager -o cat'
ssh daniel@xps.fritz.box 'sudo -l -U telegrambot'
```

Expected: the unit is `active`, the log shows `telegram bot started`, and `sudo -l -U telegrambot` lists exactly ten permitted commands with no wildcards.

Confirm the bot opens no listening socket:

```bash
ssh daniel@xps.fritz.box 'sudo ss -lntp | grep telegram-bot || echo "no listening socket, as intended"'
```

Confirm the bot user has no Docker access:

```bash
ssh daniel@xps.fritz.box 'getent group docker'
```

Expected: `telegrambot` is **not** a member.

- [ ] **Step 10: Verify idempotency**

Run the playbook again. Expected: `changed=0` and no handler fires. If the build task reports changed on a second run, the `when:` guard is wrong — fix it before committing.

- [ ] **Step 11: Commit**

```bash
git add roles/telegrambot playbooks/xps.yml
git commit -m "Deploy the Telegram bot as a systemd service

Runs as an unprivileged telegrambot user, built from source on the host so the
repository stays the single source of truth and no binary is committed.

The privilege model is ten exact sudo lines generated from the role's allowlist,
with no wildcards, so sudo itself is the boundary. The user is deliberately not
in the docker group, because socket access is root and this service takes
instructions from the internet. The sudoers file is installed with visudo
validation, since a malformed one would break sudo for every user including the
one needed to fix it.

NoNewPrivileges is deliberately not set on the unit: it blocks setuid binaries
including sudo, and the resulting failure reads like a permissions bug.

The bot opens no listening socket and writes no metrics. Its liveness comes from
systemd's own view of the unit, added in the next commit -- that needs no port
and no cooperation from the process being watched, and a wedged bot still
accepting connections would pass a scrape while being useless."
```

---

### Task 6: Systemd unit monitoring, backup path, and live verification

**Files:**
- Modify: `roles/prometheus/templates/prometheus.yml.j2`
- Modify: `roles/backup/defaults/main.yml`
- Modify: `docs/superpowers/specs/2026-08-21-telegram-bot-design.md`

- [ ] **Step 1: Enable node-exporter's systemd collector**

In `roles/prometheus/tasks/main.yml`, replace the contents written by the task
named `Enable the node-exporter textfile collector` so the args line reads:

```yaml
- name: Enable the node-exporter textfile and systemd collectors
  ansible.builtin.copy:
    content: |
      # The systemd collector reports each unit's state, which is how
      # oneshot units and background services on this host are monitored.
      # Nothing has to expose a port or cooperate: a service that is wedged
      # but still accepting connections would pass an HTTP scrape while
      # failing this check.
      #
      # unit-include is deliberately narrow. Without it the collector exports
      # a series per unit on the host, which is hundreds of series carrying no
      # information anyone will ever alert on.
      NODE_EXPORTER_ARGS="--collector.textfile.directory=/var/lib/node_exporter/textfile_collector --collector.systemd --collector.systemd.unit-include=(telegram-bot|restic-backup|backup-alert|image-update-check|docker)\\.(service|timer)"
    dest: /etc/conf.d/prometheus-node-exporter
    owner: root
    group: root
    mode: '0644'
  notify: Restart node-exporter
```

- [ ] **Step 2: Verify the metric exists before writing a rule against it**

Deploy the prometheus role, then confirm the metric name and label values are
what the rule will assume. Do not skip this: a rule written against a guessed
metric name fires never and looks healthy.

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
ssh daniel@xps.fritz.box 'curl -s http://localhost:9100/metrics | grep "^node_systemd_unit_state" | head -20'
```

Expected: `node_systemd_unit_state{name="telegram-bot.service",state="active"} 1`
and siblings for the other states and units. Record the exact output in your
report. If the metric is absent or named differently, stop and correct the rule
in the next step to match reality.

- [ ] **Step 3: Write the failing rule test**

Create `tests/rules/systemd_test.yml`:

```yaml
rule_files:
  - ../../roles/prometheus/files/rules/systemd.yml

evaluation_interval: 1m

tests:
  # A unit that is active stays silent.
  - interval: 1m
    input_series:
      - series: 'node_systemd_unit_state{instance="xps",job="node",name="telegram-bot.service",state="active"}'
        values: '1+0x40'
    alert_rule_test:
      - eval_time: 30m
        alertname: SystemdUnitFailed
        exp_alerts: []

  # A unit that stops being active fires after the for: elapses.
  - interval: 1m
    input_series:
      - series: 'node_systemd_unit_state{instance="xps",job="node",name="telegram-bot.service",state="active"}'
        values: '0+0x40'
    alert_rule_test:
      - eval_time: 5m
        alertname: SystemdUnitFailed
        exp_alerts: []
      - eval_time: 20m
        alertname: SystemdUnitFailed
        exp_alerts:
          - exp_labels:
              severity: warning
              instance: xps
              job: node
              name: telegram-bot.service
              state: active
            exp_annotations:
              summary: "telegram-bot.service is not running"
              description: "The systemd unit telegram-bot.service has not been in the active state for over 15 minutes. Run `systemctl status telegram-bot.service` and `journalctl -u telegram-bot.service` on the host."

  # A oneshot unit is inactive between runs, which is normal and must not fire.
  # This is why the rule matches on failed rather than on not-active for timers
  # and oneshots.
  - interval: 1m
    input_series:
      - series: 'node_systemd_unit_state{instance="xps",job="node",name="restic-backup.service",state="active"}'
        values: '0+0x40'
      - series: 'node_systemd_unit_state{instance="xps",job="node",name="restic-backup.service",state="failed"}'
        values: '0+0x40'
    alert_rule_test:
      - eval_time: 30m
        alertname: SystemdUnitFailed
        exp_alerts: []

  # A oneshot unit that actually failed does fire.
  - interval: 1m
    input_series:
      - series: 'node_systemd_unit_state{instance="xps",job="node",name="image-update-check.service",state="failed"}'
        values: '1+0x40'
    alert_rule_test:
      - eval_time: 20m
        alertname: SystemdUnitInFailedState
        exp_alerts:
          - exp_labels:
              severity: warning
              instance: xps
              job: node
              name: image-update-check.service
              state: failed
            exp_annotations:
              summary: "image-update-check.service is in the failed state"
              description: "The systemd unit image-update-check.service has been in the failed state for over 15 minutes. Run `systemctl status image-update-check.service` and `journalctl -u image-update-check.service` on the host."
```

- [ ] **Step 4: Run to verify it fails**

Run: `./tests/run-promtool.sh`
Expected: FAIL — `roles/prometheus/files/rules/systemd.yml` does not exist.

- [ ] **Step 5: Write the rule**

Create `roles/prometheus/files/rules/systemd.yml`:

```yaml
groups:
  - name: systemd
    rules:
      # Two rules rather than one, because "not running" means opposite things
      # for the two kinds of unit here.
      #
      # telegram-bot.service is long-running: not being active IS the fault.
      # restic-backup.service and image-update-check.service are oneshot units
      # triggered by timers, and spend almost all their time inactive, which is
      # entirely healthy. Alerting on not-active would fire constantly for them.
      - alert: SystemdUnitFailed
        expr: node_systemd_unit_state{name="telegram-bot.service",state="active"} == 0
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.name }} is not running"
          description: "The systemd unit {{ $labels.name }} has not been in the active state for over 15 minutes. Run `systemctl status {{ $labels.name }}` and `journalctl -u {{ $labels.name }}` on the host."

      # Covers every watched unit, oneshot included. This is what closes the
      # recorded gap that a failing backup-alert.service notified nobody.
      - alert: SystemdUnitInFailedState
        expr: node_systemd_unit_state{state="failed"} == 1
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.name }} is in the failed state"
          description: "The systemd unit {{ $labels.name }} has been in the failed state for over 15 minutes. Run `systemctl status {{ $labels.name }}` and `journalctl -u {{ $labels.name }}` on the host."
```

- [ ] **Step 6: Run to verify it passes**

Run: `./tests/run-promtool.sh`
Expected: PASS, and the rule count for `systemd.yml` reported as 2.

- [ ] **Step 7: Mutation-verify**

Change `== 0` to `== 1` in `SystemdUnitFailed`, run `./tests/run-promtool.sh`, and
confirm the test FAILS. Restore and confirm it passes. Record both outcomes.

- [ ] **Step 8: Add the inbox to the backup paths**

In `roles/backup/defaults/main.yml`, add to `backup_paths`:

```yaml
  - /mnt/storage/telegram-inbox
```

The inbox is temporary storage, but backing it up is nearly free — restic
deduplicates by content, so once a file is moved to an already-backed-up path
both snapshots reference the same chunks — and it covers the window between
arriving and being filed.

- [ ] **Step 9: Deploy and confirm the rule sees the unit**

```bash
ansible-playbook -i inventory/hosts.ini playbooks/xps.yml
ssh daniel@xps.fritz.box 'curl -s --get http://localhost:9090/api/v1/query --data-urlencode "query=node_systemd_unit_state{name=\"telegram-bot.service\",state=\"active\"}" | jq -r ".data.result[0].value[1]"'
```

Expected: `1`.

Then prove the alert would actually fire, rather than trusting the unit test
alone:

```bash
ssh daniel@xps.fritz.box 'sudo systemctl stop telegram-bot && sleep 90'
ssh daniel@xps.fritz.box 'curl -s --get http://localhost:9090/api/v1/query --data-urlencode "query=node_systemd_unit_state{name=\"telegram-bot.service\",state=\"active\"}" | jq -r ".data.result[0].value[1]"'
ssh daniel@xps.fritz.box 'sudo systemctl start telegram-bot'
```

Expected: `0` while stopped, back to `1` after starting. The alert has a 15
minute `for:`, so it will not fire during this check — the point is confirming
the metric tracks reality, which is what the rule depends on.

- [ ] **Step 10: Verify the bot end to end from Telegram**

Send each of these from the phone and record the exact replies:

1. `/status` — expect containers, disk, backup age and alerts
2. `/restart kavita` — expect `✅ restarted kavita`, and confirm with
   `ssh daniel@xps.fritz.box 'docker ps --filter name=^kavita$ --format "{{.Status}}"'`
   showing a fresh uptime
3. `/restart caddy` — expect a refusal naming the allowed list, and confirm
   caddy was **not** restarted: its uptime must be unchanged, and
   `https://books.home.danmidwood.com` must still return 200
4. `/backup_now` — expect `✅ backup started`; confirm with
   `ssh daniel@xps.fritz.box 'systemctl is-active restic-backup.service'`
5. Send a photo and a document — expect `📎 saved as …`, and confirm both
   landed in `/mnt/storage/telegram-inbox` with sanitised names
6. `/nonsense` — expect the help text

Step 3 is the one that matters most: it proves the allowlist is enforced against
a real message, not just in a unit test.

- [ ] **Step 11: Verify the offset survives a restart**

```bash
ssh daniel@xps.fritz.box 'sudo cat /var/lib/telegram-bot/offset'
ssh daniel@xps.fritz.box 'sudo systemctl restart telegram-bot && sleep 5'
ssh daniel@xps.fritz.box 'docker ps --filter name=^kavita$ --format "{{.Status}}"'
```

Expected: the offset file holds a number, and after the restart kavita's uptime
is **unchanged** — proving the earlier `/restart kavita` was not replayed. This
is the check that the most dangerous failure mode is actually closed.

- [ ] **Step 12: Update the spec**

In `docs/superpowers/specs/2026-08-21-telegram-bot-design.md`:

1. Replace the heartbeat-and-`TelegramBotDown` description in "Failure modes"
   with what was built: node-exporter's systemd collector, and the
   `SystemdUnitFailed` and `SystemdUnitInFailedState` rules. Give both reasons
   the textfile heartbeat was rejected — that directory also holds
   `restic_backup.prom` and `smart.prom`, so write access there would let a
   compromised bot forge the backup and disk metrics — and why a scrape
   endpoint was rejected too: it would need a listening socket on all
   interfaces, and a wedged bot still accepting connections would pass a scrape
   while being useless.
2. Note that the same collector closes the phase 1 gap recorded against
   `backup-alert.service` having no `OnFailure=` of its own, and now also covers
   `restic-backup.service` and `image-update-check.service`.
3. Update the deliverables list: `roles/prometheus` gains the systemd collector
   flags and `systemd.yml`, and the bot itself exposes no network listener.

Also update the phase 1 spec, `docs/superpowers/specs/2026-08-19-observability-alerting-design.md`:
remove the known gap saying a failed unit notifies nobody, since it is now
false, and note in its place that the systemd collector is enabled with a
narrow `unit-include`.

- [ ] **Step 13: Commit**

```bash
git add roles/prometheus roles/backup docs/superpowers/specs/2026-08-21-telegram-bot-design.md
git commit -m "Watch systemd unit state, and back up the telegram inbox

node-exporter's systemd collector is enabled with a narrow unit-include, and two
rules alert on it: one for the bot not being active, one for any watched unit in
the failed state.

The spec called for a heartbeat written to the textfile collector. That
directory also holds the backup and SMART metrics, so granting write access
would let a compromised bot forge exactly the signals the alerting exists to
provide. Having the bot serve /metrics was rejected too: it needs a listening
socket on all interfaces, and a service that is wedged but still accepting
connections passes a scrape while being useless. systemd already knows whether
the unit is running.

This also closes a gap recorded in the phase 1 design, that a failing
backup-alert.service notified nobody, and extends the same coverage to
restic-backup.service and image-update-check.service.

The telegram inbox joins backup_paths: restic deduplicates by content, so once a
file is moved to an already-backed-up path both snapshots share its chunks, and
this covers the window before it is filed."
```

---

## Self-Review

**Spec coverage.** Privilege model → Task 5 (sudoers template, user creation, unit). `/restart` allowlist → Tasks 3 and 5, verified live in Task 6 Step 4.3. `/status` from Prometheus and Alertmanager → Task 2. `/backup_now` with `--no-block` → Task 3. File inbox with 20MB cap and hostile-filename handling → Task 4. Inbox in `backup_paths` → Task 6. Offset persistence → Task 1, verified live in Task 6 Step 11. Liveness monitoring → Task 6 Steps 1 to 7. HTML escaping → Tasks 2, 3 and 4, tested in Task 2. Unauthorised senders ignored silently → Task 1 (`Poll` logs and does not reply).

**Recorded deviation.** The spec's heartbeat-plus-`TelegramBotDown` is replaced by node-exporter's systemd collector and two rules, for the reasons given at the top of this plan. Task 6 Step 12 updates both specs rather than leaving them disagreeing.

**Placeholder scan.** No TBDs. Every value — the image of the toolchain, the sudo behaviour, the scrape addressing convention, the secret names already in `user_passwords.yml` — was verified on the host before this plan was written and is listed under "Facts Already Verified".

**Type consistency.** `Bot`, `Client`, `Runner`, `Message`, `Update`, `User`, `Document`, `PhotoSize` are declared once in Task 1 and used unchanged afterwards. `stubs.go` holds four stubs, replaced one per task, and Task 4 deletes the file — Task 4 Step 4 verifies it is gone, and Task 5 would not compile if it were not. The env var names in `main.go` (Task 5 Step 1) match `env.j2` (Task 5 Step 4) exactly: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_ALLOWED_USER_ID`, `TELEGRAM_RESTARTABLE`, `TELEGRAM_INBOX_DIR`, `TELEGRAM_STATE_FILE`, `PROMETHEUS_URL`, `ALERTMANAGER_URL`. The bot has no metrics code and opens no socket.

**One consistency risk called out.** The sudoers template grants `/usr/bin/systemctl start --no-block restic-backup.service` and `actions.go` invokes exactly `sudo -n /usr/bin/systemctl start --no-block restic-backup.service`. These two must match token for token or sudo refuses. Task 5 Step 11 verifies the real grants with `sudo -l -U telegrambot`, and Task 6 Step 4.4 exercises the path for real.
