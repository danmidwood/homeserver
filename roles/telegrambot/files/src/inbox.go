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
