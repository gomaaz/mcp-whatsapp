package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/mediacache"
	"github.com/sealjay/mcp-whatsapp/internal/security"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

func TestParseRetention(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"0", 0, false},
		{"0d", 0, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"1h", time.Hour, false},
		{" 7d ", 7 * 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"30m", 0, true}, // under the minimum
		{"30", 0, true},  // no unit
		{"-1d", 0, true},
		{"-5h", 0, true},
		{"1.5d", 0, true},
		{"d", 0, true},
		{"abc", 0, true},
		{"99999d", 0, true},
	}
	for _, tc := range cases {
		got, err := parseRetention(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseRetention(%q) = %v, %v; want %v, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// pruneStore lays out a store with the files prune must keep and one old
// cached media file it may delete.
func pruneStore(t *testing.T) (dir, oldMedia string, keep []string) {
	t.Helper()
	dir = t.TempDir()
	old := time.Now().Add(-90 * 24 * time.Hour)
	write := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	keep = []string{
		filepath.Join(dir, "messages.db"),
		filepath.Join(dir, "whatsapp.db"),
		filepath.Join(dir, "uploads", "out.pdf"),
	}
	oldMedia = filepath.Join(dir, "447700000001@s.whatsapp.net", hex.EncodeToString([]byte("M1")), "photo.jpg")
	for _, p := range append(keep, oldMedia) {
		write(p)
	}
	return dir, oldMedia, keep
}

func TestRunPrune_DeletesOldMediaAndKeepsTheRest(t *testing.T) {
	t.Setenv("WHATSAPP_MCP_MEDIA_ROOT", "")
	dir, oldMedia, keep := pruneStore(t)
	if code := runPrune(dir, []string{"-media-older-than", "30d", "-dry-run"}); code != 0 {
		t.Fatalf("dry run exit code %d", code)
	}
	if _, err := os.Stat(oldMedia); err != nil {
		t.Fatalf("dry run deleted media: %v", err)
	}
	if code := runPrune(dir, []string{"-media-older-than", "30d"}); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if _, err := os.Stat(oldMedia); !os.IsNotExist(err) {
		t.Errorf("old media not deleted (err %v)", err)
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s deleted: %v", p, err)
		}
	}
}

// While serve holds the store lock, prune refuses to delete; a dry run is
// still allowed because it only reads.
func TestRunPrune_RefusesWhileServeHoldsLock(t *testing.T) {
	dir, oldMedia, _ := pruneStore(t)
	lock, err := store.TryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if code := runPrune(dir, []string{"-media-older-than", "30d"}); code != 1 {
		t.Errorf("exit code %d while locked, want 1", code)
	}
	if _, err := os.Stat(oldMedia); err != nil {
		t.Fatalf("prune deleted media while serve held the lock: %v", err)
	}
	if code := runPrune(dir, []string{"-media-older-than", "30d", "-dry-run"}); code != 0 {
		t.Errorf("dry run exit code %d while locked, want 0", code)
	}
}

func TestRunPrune_FlagErrors(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		nil,
		{"-media-older-than", "0"},
		{"-media-older-than", "10m"},
		{"-media-older-than", "30d", "extra"},
	} {
		if code := runPrune(dir, args); code != 2 {
			t.Errorf("runPrune(%q) exit code %d, want 2", args, code)
		}
	}
	missing := filepath.Join(dir, "nope")
	if code := runPrune(missing, []string{"-media-older-than", "30d"}); code != 1 {
		t.Errorf("missing store: exit code %d, want 1", code)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("prune created the missing store directory")
	}
}

func TestMediaRetentionLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		mediaRetentionLoop(ctx, 5*time.Millisecond, func() {
			if n.Add(1) == 3 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
	if got := n.Load(); got < 3 {
		t.Errorf("prune ran %d times, want at least 3 (startup + ticks)", got)
	}
}

func TestMediaRetentionLoop_RunsAtStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var n atomic.Int32
	mediaRetentionLoop(ctx, time.Hour, func() { n.Add(1) })
	if n.Load() != 1 {
		t.Errorf("prune ran %d times, want exactly once at startup", n.Load())
	}
}

// A cached file serve's retention cannot delete (on Windows typically one
// another program has open) must not put the chat's phone number, the
// message ID folder or the attachment name into the warning.
func TestRetainMedia_FailureLogsNoPaths(t *testing.T) {
	dir := t.TempDir()
	const chat = "491751234567@s.whatsapp.net"
	msgDir := hex.EncodeToString([]byte("3EB0C0FFEE0123456789"))
	file := filepath.Join(dir, chat, msgDir, "Rechnung_Mueller.pdf")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
	} else {
		if err := os.Chmod(filepath.Dir(file), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(file), 0o700) })
		if err := os.WriteFile(filepath.Join(filepath.Dir(file), "probe"), nil, 0o600); err == nil {
			t.Skip("a read-only directory does not stop deletion here (root?)")
		}
	}

	var buf bytes.Buffer
	retainMedia(&buf, &security.Redactor{}, 30*24*time.Hour, func(d time.Duration, dry bool) (mediacache.Report, error) {
		return mediacache.Prune(dir, mediacache.Options{OlderThan: d, DryRun: dry})
	})
	line := buf.String()
	if !strings.HasPrefix(line, "warn: media retention: ") || strings.Count(line, "\n") != 1 {
		t.Fatalf("log = %q, want one warning line", line)
	}
	for _, leak := range []string{"491751234567", chat, msgDir, "Rechnung_Mueller.pdf", "Rechnung"} {
		if strings.Contains(line, leak) {
			t.Errorf("log contains %q: %q", leak, line)
		}
	}
	if regexp.MustCompile(`\d{5,}`).MatchString(line) {
		t.Errorf("log contains a digit run: %q", line)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("locked file is gone: %v", err)
	}
}

// Should an error with a path reach retainMedia anyway, the redactor still
// masks the phone number in it.
func TestRetainMedia_RedactsErrors(t *testing.T) {
	var buf bytes.Buffer
	retainMedia(&buf, &security.Redactor{}, time.Hour, func(time.Duration, bool) (mediacache.Report, error) {
		return mediacache.Report{}, errors.New(`remove C:\store\491751234567@s.whatsapp.net\ab\x.pdf: busy`)
	})
	if strings.Contains(buf.String(), "491751234567") {
		t.Errorf("log = %q, want the phone number masked", buf.String())
	}

	buf.Reset()
	retainMedia(&buf, &security.Redactor{}, time.Hour, func(time.Duration, bool) (mediacache.Report, error) {
		return mediacache.Report{Files: 2, Bytes: 2048}, nil
	})
	if got, want := buf.String(), "media retention: deleted 2 file(s), 2.0 KiB\n"; got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}
