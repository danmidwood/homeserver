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

// CallbackQuery arrives when an inline keyboard button is pressed. It is not a
// message, which is why the bot has to ask Telegram for this update type
// explicitly -- without it, button presses simply never appear.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

type Update struct {
	UpdateID int            `json:"update_id"`
	Message  *Message       `json:"message"`
	Callback *CallbackQuery `json:"callback_query"`
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
	// Messages and button presses. Anything not listed here is never delivered,
	// so a missing entry looks exactly like a bot that ignores you.
	form.Set("allowed_updates", `["message","callback_query"]`)

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
