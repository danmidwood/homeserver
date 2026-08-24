package main

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveDoorbellDate(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	cases := []struct{ in, want string }{
		{"", today},
		{"today", today},
		{"TODAY", today},
		{"yesterday", yesterday},
		{" Yesterday ", yesterday},
		{"2026-08-22", "2026-08-22"},
	}
	for _, c := range cases {
		got, err := resolveDoorbellDate(c.in)
		if err != nil {
			t.Errorf("resolveDoorbellDate(%q): %v", c.in, err)
			continue
		}
		if got.Format("2006-01-02") != c.want {
			t.Errorf("resolveDoorbellDate(%q) = %s, want %s", c.in, got.Format("2006-01-02"), c.want)
		}
	}
	if _, err := resolveDoorbellDate("last tuesday"); err == nil {
		t.Error("an unparseable date was accepted")
	}
}

func TestStampAndPrettyTime(t *testing.T) {
	p := "/mnt/storage/ftp/data/ftpuser/2026/08/22/Reolink Video Doorbell_00_20260822093347.mp4"
	if got := stampOf(p); got != "20260822093347" {
		t.Errorf("stampOf = %q", got)
	}
	if got := prettyTime("20260822093347"); got != "09:33:47" {
		t.Errorf("prettyTime = %q", got)
	}
	// Filenames without the expected shape must not panic or produce nonsense.
	if got := stampOf("/tmp/whatever.mp4"); got != "" {
		t.Errorf("stampOf on an unexpected name = %q, want empty", got)
	}
	if got := prettyTime("short"); got != "short" {
		t.Errorf("prettyTime on a short stamp = %q", got)
	}
}

func TestSecondsOfDay(t *testing.T) {
	if got := secondsOfDay("20260822093347"); got != 9*3600+33*60+47 {
		t.Errorf("secondsOfDay = %d", got)
	}
	if got := secondsOfDay("bad"); got != -1 {
		t.Errorf("secondsOfDay on a malformed stamp = %d, want -1", got)
	}
}

// callback_data is capped at 64 bytes by Telegram. A clip is identified by its
// timestamp precisely so this cannot be exceeded; the test guards the choice.
func TestButtonsStayWithinCallbackLimit(t *testing.T) {
	clips := []doorbellClip{
		{Stamp: "20260822093347", Still: "/x.jpg", Duration: 10},
		{Stamp: "20260822093508", Still: "/y.jpg", Duration: 8},
	}
	rows := buttonsFor(clips)
	for _, r := range rows {
		for _, b := range r {
			if len(b.Data) > 64 {
				t.Errorf("callback data %q is %d bytes", b.Data, len(b.Data))
			}
			if !strings.HasPrefix(b.Data, "p:") {
				t.Errorf("callback data %q lacks its prefix", b.Data)
			}
		}
	}
	if _, err := keyboardJSON(rows); err != nil {
		t.Errorf("keyboardJSON: %v", err)
	}
}

// A clip with no still cannot be shown on the sheet, so it must not get a
// button either -- a button with nothing above it is a button for a tile that
// is not there, which would point at the wrong image.
func TestClipsWithoutStillsGetNoButton(t *testing.T) {
	clips := []doorbellClip{
		{Stamp: "20260822093347", Still: "/x.jpg"},
		{Stamp: "20260822093508", Still: ""},
		{Stamp: "20260822093526", Still: "/z.jpg"},
	}
	n := 0
	for _, r := range buttonsFor(clips) {
		n += len(r)
	}
	if n != 2 {
		t.Errorf("got %d buttons, want 2 (the still-less clip must be skipped)", n)
	}
}

func TestButtonsLaidOutFourPerRow(t *testing.T) {
	var clips []doorbellClip
	for i := 0; i < 9; i++ {
		clips = append(clips, doorbellClip{Stamp: "2026082209334" + string(rune('0'+i)), Still: "/s.jpg"})
	}
	rows := buttonsFor(clips)
	if len(rows) != 3 {
		t.Fatalf("got %d rows for 9 buttons, want 3", len(rows))
	}
	for i, r := range rows[:2] {
		if len(r) != 4 {
			t.Errorf("row %d has %d buttons, want 4", i, len(r))
		}
	}
	if len(rows[2]) != 1 {
		t.Errorf("last row has %d buttons, want 1", len(rows[2]))
	}
}

func TestKeyboardRejectsOversizedData(t *testing.T) {
	rows := [][]InlineButton{{{Text: "x", Data: strings.Repeat("a", 65)}}}
	if _, err := keyboardJSON(rows); err == nil {
		t.Error("65 bytes of callback data was accepted; Telegram caps it at 64")
	}
}

// The sheet must survive a still that will not decode: around one upload in
// 450 arrives empty or truncated, and one bad file should not cost the whole
// day's view.
func TestBuildSheetSkipsUndecodableStills(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jpg")
	writeTestJPEG(t, good, 640, 480)
	bad := filepath.Join(dir, "bad.jpg")
	if err := os.WriteFile(bad, []byte("not a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.jpg")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "sheet.jpg")
	n, err := buildSheet([]string{good, bad, empty, good}, out)
	if err != nil {
		t.Fatalf("buildSheet: %v", err)
	}
	if n != 2 {
		t.Errorf("placed %d tiles, want 2 (the undecodable ones skipped)", n)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Error("no sheet was written")
	}
}

func TestBuildSheetWithNothingUsable(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.jpg")
	os.WriteFile(bad, []byte("nope"), 0o644)
	n, err := buildSheet([]string{bad}, filepath.Join(dir, "s.jpg"))
	if err != nil {
		t.Fatalf("buildSheet: %v", err)
	}
	if n != 0 {
		t.Errorf("placed %d tiles, want 0", n)
	}
}

// Portrait and landscape stills in one sheet must not overlap; rows take the
// height of their tallest tile.
func TestBuildSheetHandlesMixedAspectRatios(t *testing.T) {
	dir := t.TempDir()
	wide := filepath.Join(dir, "wide.jpg")
	tall := filepath.Join(dir, "tall.jpg")
	writeTestJPEG(t, wide, 800, 200)
	writeTestJPEG(t, tall, 200, 800)

	out := filepath.Join(dir, "sheet.jpg")
	n, err := buildSheet([]string{wide, tall, wide, tall, wide}, out)
	if err != nil || n != 5 {
		t.Fatalf("buildSheet: n=%d err=%v", n, err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatalf("sheet is not a valid jpeg: %v", err)
	}
	// Two rows: the first is as tall as the tall tile, the second holds one
	// wide tile. A naive fixed row height would come out far shorter.
	if cfg.Height < 1000 {
		t.Errorf("sheet is %dpx tall; rows do not appear to size to their tallest tile", cfg.Height)
	}
}

func writeTestJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 0x80, 0xff})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
}
