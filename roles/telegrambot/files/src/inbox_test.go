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
		{"emoji.png", "emoji.png"},
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
	for _, in := range []string{"../../etc/passwd", "/etc/shadow", "..", "....//....//x", `..\..\x`} {
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

	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(data) != "first" {
		t.Error("the existing file was modified")
	}
}
