package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// InlineButton is one key of an inline keyboard. Data is returned verbatim in
// a callback_query when it is pressed, and Telegram caps it at 64 bytes -- which
// is why clips are identified by their 14-character timestamp rather than by
// filename.
type InlineButton struct {
	Text string
	Data string
}

func keyboardJSON(rows [][]InlineButton) (string, error) {
	type btn struct {
		Text string `json:"text"`
		Data string `json:"callback_data"`
	}
	out := make([][]btn, 0, len(rows))
	for _, r := range rows {
		row := make([]btn, 0, len(r))
		for _, b := range r {
			if len(b.Data) > 64 {
				return "", fmt.Errorf("callback data too long: %q", b.Data)
			}
			row = append(row, btn{Text: b.Text, Data: b.Data})
		}
		out = append(out, row)
	}
	buf, err := json.Marshal(map[string]any{"inline_keyboard": out})
	return string(buf), err
}

// upload posts a file with multipart/form-data, which is how Telegram takes
// photos and videos. The alternative is a URL it can fetch, which is no use for
// files that only exist on this host.
func (c *Client) upload(ctx context.Context, method, field, path string, fields map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	part, err := w.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(c.BaseURL, "/"), c.Token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var ar apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return fmt.Errorf("%s: decoding response: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !ar.OK {
		return fmt.Errorf("%s: telegram returned %d: %s", method, resp.StatusCode, ar.Description)
	}
	return nil
}

func (c *Client) SendPhoto(ctx context.Context, chatID int64, path, caption string, rows [][]InlineButton) error {
	fields := map[string]string{
		"chat_id": strconv.FormatInt(chatID, 10),
		"caption": html.EscapeString(caption),
		// Consistent with every other message this bot sends; a stray angle
		// bracket in an unescaped caption would make Telegram drop it.
		"parse_mode": "HTML",
	}
	if len(rows) > 0 {
		kb, err := keyboardJSON(rows)
		if err != nil {
			return err
		}
		fields["reply_markup"] = kb
	}
	return c.upload(ctx, "sendPhoto", "photo", path, fields)
}

func (c *Client) SendVideo(ctx context.Context, chatID int64, path, caption string) error {
	return c.upload(ctx, "sendVideo", "video", path, map[string]string{
		"chat_id":    strconv.FormatInt(chatID, 10),
		"caption":    html.EscapeString(caption),
		"parse_mode": "HTML",
		// Without this Telegram sends it as a file attachment rather than
		// something playable in the chat.
		"supports_streaming": "true",
	})
}

// AnswerCallback clears the loading spinner on the pressed button. Telegram
// keeps showing it until this is called, so skipping it makes the bot look
// hung even when it is working.
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	form := map[string][]string{
		"callback_query_id": {id},
	}
	if text != "" {
		form["text"] = []string{text}
	}
	_, err := c.call(ctx, "answerCallbackQuery", form)
	return err
}

func htmlEscape(s string) string { return html.EscapeString(s) }
