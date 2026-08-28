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

// Every clip gets a button, including one with no still: the clip is playable
// whether or not a picture of it arrived. The sheet draws a placeholder tile in
// its place, so the button still sits under the tile it plays.
func TestEveryClipGetsAButton(t *testing.T) {
	clips := []doorbellClip{
		{Stamp: "20260822093347", Still: "/x.jpg"},
		{Stamp: "20260822093508", Still: ""},
		{Stamp: "20260822093526", Still: "/z.jpg"},
	}
	n := 0
	for _, r := range buttonsFor(clips) {
		n += len(r)
	}
	if n != 3 {
		t.Errorf("got %d buttons, want 3 (the still-less clip must still be playable)", n)
	}
}

// One tile per clip regardless of how many stills decoded, or the buttons
// beneath the sheet stop lining up with the pictures above them.
func TestSheetDrawsATilePerClipIncludingPlaceholders(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jpg")
	writeTestJPEG(t, good, 640, 360)
	out := filepath.Join(dir, "sheet.jpg")

	// Two real stills and two clips without one.
	placed, err := buildSheet([]string{good, "", good, ""}, out)
	if err != nil {
		t.Fatalf("buildSheet: %v", err)
	}
	if placed != 2 {
		t.Errorf("reported %d pictures, want 2", placed)
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
	// Four tiles across, so the sheet must be full width even though only two
	// of them carried a picture.
	if want := 4*tileWidth + 5*tilePad; cfg.Width != want {
		t.Errorf("sheet is %dpx wide, want %d (four tiles)", cfg.Width, want)
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

// A still's filename timestamp can land either side of its clip's. The archive
// has stills at +4, +3 and -3 seconds relative to the clip they belong to, so
// matching only forwards silently drops every event whose still was stamped
// first.
func TestStillMatchesWhenStampedBeforeItsClip(t *testing.T) {
	clips := []doorbellClip{
		{Stamp: "20260828100138"}, // its still is 3s EARLIER
		{Stamp: "20260828054217"}, // its still is 4s later
	}
	stills := []string{
		"/a/Doorbell_00_20260828054221.jpg",
		"/a/Doorbell_00_20260828100135.jpg",
	}
	assignStills(clips, stills)

	if clips[0].Still == "" {
		t.Error("clip 10:01:38 got no still; 10:01:35 is 3s earlier and belongs to it")
	}
	if clips[1].Still == "" {
		t.Error("clip 05:42:17 got no still; 05:42:21 is 4s later and belongs to it")
	}
	if clips[0].Still != "" && clips[0].Still == clips[1].Still {
		t.Error("both clips claimed the same still")
	}
}

// A still far from any clip belongs to a different event and must not be
// borrowed.
func TestDistantStillIsNotBorrowed(t *testing.T) {
	clips := []doorbellClip{{Stamp: "20260828132547"}}
	stills := []string{"/a/Doorbell_00_20260828132517.jpg"} // 30s earlier
	assignStills(clips, stills)
	if clips[0].Still != "" {
		t.Errorf("clip claimed a still 30s away: %q", clips[0].Still)
	}
}

// The caption counted every clip found while the sheet and buttons only ever
// showed the ones with a still, so /doorbell reported 8 and displayed 3.
func TestCaptionReportsWhatIsActuallyShown(t *testing.T) {
	day := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)

	got := doorbellCaption(day, 8, 4, 0, false)
	if !strings.Contains(got, "8") || !strings.Contains(got, "4") {
		t.Errorf("caption %q should say both how many were found and how many are shown", got)
	}

	full := doorbellCaption(day, 4, 4, 0, false)
	if strings.Contains(full, "no still") || strings.Contains(full, "without") {
		t.Errorf("caption %q should not qualify a complete set", full)
	}
}
