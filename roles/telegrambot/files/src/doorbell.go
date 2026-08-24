package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Doorbell clips arrive by FTP from the Reolink camera and are laid out as
// YYYY/MM/ for older recordings and YYYY/MM/DD/ for newer ones. Matching on
// the timestamp embedded in the filename covers both, and is also how the
// laptop-side tools/doorbell script finds them.
const doorbellRoot = "/mnt/storage/ftp/data/ftpuser"

// Clips shorter than this are almost always motion crossing the frame rather
// than anything happening. Lengths are bimodal, with a large spike at two to
// four seconds and a second population from eight upwards; eight is the trough
// between them.
const doorbellLongSeconds = 8.0

// Telegram caps an inline keyboard well below the ~100 clips a busy day
// produces, and a wall of buttons is unusable long before that.
const maxButtons = 24

type doorbellClip struct {
	Path     string
	Stamp    string // YYYYMMDDHHMMSS, the identity used in callback data
	Duration float64
	Still    string
}

// resolveDoorbellDate accepts what a person would type.
func resolveDoorbellDate(arg string) (time.Time, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "today":
		return time.Now(), nil
	case "yesterday":
		return time.Now().AddDate(0, 0, -1), nil
	}
	return time.Parse("2006-01-02", strings.TrimSpace(arg))
}

func stampOf(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if i := strings.LastIndex(base, "_"); i >= 0 {
		return base[i+1:]
	}
	return ""
}

// secondsOfDay converts the HHMMSS half of a stamp. Everything compared here
// is within one day, so the date half can be ignored.
func secondsOfDay(stamp string) int {
	if len(stamp) < 14 {
		return -1
	}
	h, _ := strconv.Atoi(stamp[8:10])
	m, _ := strconv.Atoi(stamp[10:12])
	s, _ := strconv.Atoi(stamp[12:14])
	return h*3600 + m*60 + s
}

// findDoorbellFiles returns every file for a day with the given extension.
// Both directory layouts live under YYYY/MM, so that is where it looks.
func findDoorbellFiles(day time.Time, ext string) []string {
	monthDir := filepath.Join(doorbellRoot, day.Format("2006"), day.Format("01"))
	want := day.Format("20060102")

	var out []string
	filepath.Walk(monthDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(p), ext) {
			return nil
		}
		if strings.Contains(filepath.Base(p), want) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func probeDuration(ctx context.Context, path string) float64 {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	// ffprobe prints N/A for a file it can open but cannot measure, which is
	// what a truncated upload looks like.
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return v
}

// collectClips gathers a day's clips, their durations, and the still that
// belongs to each.
//
// Stills are matched on the timestamp in the filename, not on modification
// time. The still is uploaded BEFORE its clip, being roughly ten times
// smaller, and the gap between the two uploads grows with the size of the
// video -- so upload time is not a usable key. Filename timestamps are the
// event time and sit a consistent five to seven seconds apart.
//
// Each still is claimed by at most one clip. Without that, a clip whose own
// still failed to upload borrows one from a neighbouring clip and shows a
// different event entirely, which is worse than showing none.
func collectClips(ctx context.Context, day time.Time, minDuration float64) []doorbellClip {
	stills := findDoorbellFiles(day, ".jpg")
	stillSecs := make([]int, len(stills))
	for i, s := range stills {
		stillSecs[i] = secondsOfDay(stampOf(s))
	}
	claimed := make([]bool, len(stills))

	var clips []doorbellClip
	for _, p := range findDoorbellFiles(day, ".mp4") {
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			continue // empty upload
		}
		d := probeDuration(ctx, p)
		if d <= 0 || d < minDuration {
			continue // truncated, or shorter than asked for
		}

		c := doorbellClip{Path: p, Stamp: stampOf(p), Duration: d}
		clipSec := secondsOfDay(c.Stamp)
		best := 16
		bestIdx := -1
		for i := range stills {
			if claimed[i] {
				continue
			}
			gap := stillSecs[i] - clipSec
			if gap >= 0 && gap < best {
				best, bestIdx = gap, i
			}
		}
		if bestIdx >= 0 {
			claimed[bestIdx] = true
			c.Still = stills[bestIdx]
		}
		clips = append(clips, c)
	}
	return clips
}

// contactSheet composes the stills into one grid.
//
// Tiles carry no captions: the standard library cannot draw text without a font
// dependency, and the buttons beneath the sheet already give the time of every
// clip, in the same order and four to a row, so a button sits under its tile.
func contactSheet(clips []doorbellClip, dest string) (int, error) {
	var paths []string
	for _, c := range clips {
		if c.Still != "" {
			paths = append(paths, c.Still)
		}
	}
	return buildSheet(paths, dest)
}

// 20260822093347 -> 09:33:47
func prettyTime(stamp string) string {
	if len(stamp) < 14 {
		return stamp
	}
	return stamp[8:10] + ":" + stamp[10:12] + ":" + stamp[12:14]
}

func (b *Bot) doorbell(ctx context.Context, chatID int64, arg string) string {
	fields := strings.Fields(arg)
	dateArg := ""
	minDuration := doorbellLongSeconds
	for _, f := range fields {
		if strings.EqualFold(f, "all") {
			minDuration = 0
			continue
		}
		dateArg = f
	}

	day, err := resolveDoorbellDate(dateArg)
	if err != nil {
		return "Usage: /doorbell [today|yesterday|YYYY-MM-DD] [all]"
	}

	clips := collectClips(ctx, day, minDuration)
	if len(clips) == 0 {
		if minDuration > 0 {
			return fmt.Sprintf("No clips on %s over %.0fs. Try /doorbell %s all",
				day.Format("2006-01-02"), minDuration, day.Format("2006-01-02"))
		}
		return "No clips on " + day.Format("2006-01-02") + "."
	}

	truncated := false
	if len(clips) > maxButtons {
		clips = clips[:maxButtons]
		truncated = true
	}

	sheet := filepath.Join(os.TempDir(), fmt.Sprintf("doorbell-%s.jpg", day.Format("20060102")))
	defer os.Remove(sheet)

	shown, err := contactSheet(clips, sheet)
	if err != nil {
		return "❌ could not build the contact sheet: " + htmlEscape(err.Error())
	}
	if shown == 0 {
		return fmt.Sprintf("%d clips on %s, but none has a still to show.",
			len(clips), day.Format("2006-01-02"))
	}

	caption := fmt.Sprintf("%s — %d clips", day.Format("2006-01-02"), len(clips))
	if minDuration > 0 {
		caption += fmt.Sprintf(" over %.0fs", minDuration)
	}
	if truncated {
		caption += fmt.Sprintf(" (showing the first %d)", maxButtons)
	}

	if err := b.TG.SendPhoto(ctx, chatID, sheet, caption, buttonsFor(clips)); err != nil {
		return "❌ could not send the sheet: " + htmlEscape(err.Error())
	}
	return "" // the photo is the reply
}

// buttonsFor lays the buttons out four to a row, matching the grid columns, so
// a button sits under the tile it belongs to.
func buttonsFor(clips []doorbellClip) [][]InlineButton {
	var rows [][]InlineButton
	var row []InlineButton
	for _, c := range clips {
		if c.Still == "" {
			continue
		}
		row = append(row, InlineButton{
			Text: prettyTime(c.Stamp)[:5], // HH:MM is enough to read at a glance
			Data: "p:" + c.Stamp,
		})
		if len(row) == 4 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// playClip answers a button press by sending the clip it names.
func (b *Bot) playClip(ctx context.Context, chatID int64, stamp string) string {
	if len(stamp) < 14 {
		return "That button carried no usable timestamp."
	}
	day, err := time.Parse("20060102", stamp[:8])
	if err != nil {
		return "That button carried an unreadable date."
	}
	for _, p := range findDoorbellFiles(day, ".mp4") {
		if stampOf(p) != stamp {
			continue
		}
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			return "That clip is an empty upload — there is nothing to play."
		}
		if err := b.TG.SendVideo(ctx, chatID, p, prettyTime(stamp)); err != nil {
			return "❌ could not send that clip: " + htmlEscape(err.Error())
		}
		return ""
	}
	return "That clip is no longer on the server."
}
