package mediacache

import (
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var (
	testNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	oldTime = testNow.Add(-60 * 24 * time.Hour)
	newTime = testNow.Add(-time.Hour)
)

const (
	chatA = "447700000001@s.whatsapp.net"
	chatB = "123456789@g.us"
)

// writeFile creates path (and its parents) with mtime at.
func writeFile(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data:"+filepath.Base(path)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// cached returns the cache path of a file for a message, as Download builds it.
func cached(t *testing.T, store, chat, id, name string) string {
	t.Helper()
	dir, err := MessageDir(store, chat, id)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func mustExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if !exists(p) {
			t.Errorf("%s was deleted", p)
		}
	}
}

func mustBeGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if exists(p) {
			t.Errorf("%s still exists", p)
		}
	}
}

// dirLink makes link point at the directory target: a symlink where the
// OS allows it, otherwise (Windows without developer mode) a junction.
func dirLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("cannot create junction: %v %s", err, out)
		}
	}
	t.Skip("symlinks not supported here")
}

func TestMessageDir(t *testing.T) {
	store := t.TempDir()
	got, err := MessageDir(store, "491700000000:12@s.whatsapp.net", "ABC")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(store, "491700000000_12@s.whatsapp.net", hex.EncodeToString([]byte("ABC")))
	if got != want {
		t.Errorf("MessageDir = %q, want %q", got, want)
	}
	for _, jid := range []string{"../escape@s.whatsapp.net", "", ".", "..", "a/../../b@x", `..\b@x`} {
		if _, err := MessageDir(store, jid, "M1"); !errors.Is(err, ErrEscapesStore) {
			if runtime.GOOS != "windows" && jid == `..\b@x` {
				continue // a plain file name outside Windows
			}
			t.Errorf("MessageDir(%q) error = %v, want ErrEscapesStore", jid, err)
		}
	}
}

func TestPrune_DeletesOldMediaOnly(t *testing.T) {
	store := t.TempDir()
	keep := []string{
		filepath.Join(store, "messages.db"),
		filepath.Join(store, "whatsapp.db"),
		filepath.Join(store, ".lock"),
		// Not named like a chat: never entered, however old.
		filepath.Join(store, "uploads", "received.jpg"),
		filepath.Join(store, "uploads", chatA, hex.EncodeToString([]byte("U1")), "out.jpg"),
		// Fresh file in chat A.
		cached(t, store, chatA, "NEW", "new.jpg"),
		// Unexpected names inside a chat directory are left alone.
		filepath.Join(store, chatA, "notes", "old.txt"),
		filepath.Join(store, chatA, "ABCD", "old.txt"),
		// A subdirectory inside a message directory is not cache layout.
		filepath.Join(filepath.Dir(cached(t, store, chatA, "MIX", "x")), "sub", "old.bin"),
	}
	for _, p := range keep {
		writeFile(t, p, oldTime)
	}
	writeFile(t, keep[5], newTime)

	gone := []string{
		cached(t, store, chatA, "OLD1", "photo.jpg"),
		cached(t, store, chatA, "OLD1", ".download-123"), // leftover temp file
		cached(t, store, chatA, "MIX", "old.pdf"),
		cached(t, store, chatB, "OLD2", "voice.ogg"),
	}
	for _, p := range gone {
		writeFile(t, p, oldTime)
	}
	// An empty message directory left by a failed download.
	emptyDir := filepath.Dir(cached(t, store, chatB, "EMPTY", "x"))
	if err := os.MkdirAll(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}

	rep, err := Prune(store, Options{OlderThan: 30 * 24 * time.Hour, Now: testNow})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	mustExist(t, keep...)
	mustBeGone(t, gone...)
	mustBeGone(t, filepath.Dir(gone[0]), emptyDir, filepath.Join(store, chatB))
	// chat A still holds NEW and the unexpected entries; MIX keeps "sub".
	mustExist(t, filepath.Join(store, chatA), filepath.Dir(gone[2]))

	if rep.Files != 4 {
		t.Errorf("Files = %d, want 4", rep.Files)
	}
	if rep.MessageDirs != 3 || rep.ChatDirs != 1 {
		t.Errorf("MessageDirs/ChatDirs = %d/%d, want 3/1", rep.MessageDirs, rep.ChatDirs)
	}
	if rep.Bytes <= 0 {
		t.Errorf("Bytes = %d", rep.Bytes)
	}
	if rep.Ignored != 3 { // notes, ABCD, MIX/sub
		t.Errorf("Ignored = %d, want 3", rep.Ignored)
	}
}

func TestPrune_DryRunDeletesNothing(t *testing.T) {
	store := t.TempDir()
	old := cached(t, store, chatA, "OLD", "a.jpg")
	writeFile(t, old, oldTime)
	rep, err := Prune(store, Options{OlderThan: 24 * time.Hour, Now: testNow, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	mustExist(t, old)
	if rep.Files != 1 || rep.MessageDirs != 1 || rep.ChatDirs != 1 {
		t.Errorf("dry-run report = %+v, want 1 file, 1 message dir, 1 chat dir", rep)
	}
}

func TestPrune_SkipsMediaRoot(t *testing.T) {
	store := t.TempDir()
	// A media root that happens to be named like a chat directory.
	root := filepath.Join(store, "media@root")
	inRoot := filepath.Join(root, hex.EncodeToString([]byte("M")), "keep.jpg")
	writeFile(t, inRoot, oldTime)
	if _, err := Prune(store, Options{OlderThan: time.Hour, Now: testNow, Skip: []string{root}}); err != nil {
		t.Fatal(err)
	}
	mustExist(t, inRoot)

	if _, err := Prune(store, Options{OlderThan: time.Hour, Now: testNow, Skip: []string{store}}); err == nil {
		t.Error("Prune accepted the store itself as the media root")
	}
	if _, err := Prune(store, Options{OlderThan: time.Hour, Now: testNow, Skip: []string{filepath.Dir(store)}}); err == nil {
		t.Error("Prune accepted a media root that contains the store")
	}
}

func TestPrune_RequiresPositiveAge(t *testing.T) {
	if _, err := Prune(t.TempDir(), Options{}); err == nil {
		t.Error("Prune ran without an age")
	}
}

// Links inside the store that point outside it must never lead to files
// outside being deleted.
func TestPrune_DoesNotFollowLinksOutOfStore(t *testing.T) {
	store := t.TempDir()
	outside := t.TempDir()

	// 1. A whole chat directory that is a link to an outside tree laid out
	//    like the cache.
	outChat := filepath.Join(outside, "chat")
	victim1 := filepath.Join(outChat, hex.EncodeToString([]byte("V1")), "secret.pdf")
	writeFile(t, victim1, oldTime)
	dirLink(t, outChat, filepath.Join(store, "evil@s.whatsapp.net"))

	// 2. A message directory that is a link, inside a real chat directory.
	outMsg := filepath.Join(outside, "msg")
	victim2 := filepath.Join(outMsg, "secret2.pdf")
	writeFile(t, victim2, oldTime)
	real := filepath.Dir(cached(t, store, chatA, "REAL", "x"))
	writeFile(t, filepath.Join(real, "old.jpg"), oldTime)
	dirLink(t, outMsg, filepath.Join(store, chatA, hex.EncodeToString([]byte("LINK"))))

	// 3. A file symlink inside a message directory (needs symlink rights).
	victim3 := filepath.Join(outside, "secret3.pdf")
	writeFile(t, victim3, oldTime)
	fileLink := filepath.Join(real, "link.pdf")
	_ = os.Symlink(victim3, fileLink)

	if _, err := Prune(store, Options{OlderThan: time.Hour, Now: testNow}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	mustExist(t, victim1, victim2, victim3, outChat, outMsg)
	mustBeGone(t, filepath.Join(real, "old.jpg"))
	// The links themselves stay too: they are not part of the layout.
	mustExist(t, filepath.Join(store, "evil@s.whatsapp.net"), filepath.Join(store, chatA, hex.EncodeToString([]byte("LINK"))))
}

// A store directory that is itself reached through a link is fine: the
// layout below it is what counts.
func TestPrune_StoreBehindLink(t *testing.T) {
	realStore := t.TempDir()
	old := cached(t, realStore, chatA, "OLD", "a.jpg")
	writeFile(t, old, oldTime)
	link := filepath.Join(t.TempDir(), "store")
	// Only a real symlink: Go's EvalSymlinks does not resolve paths through
	// Windows junctions, so a store behind a junction is left alone (fails
	// closed) rather than pruned.
	if err := os.Symlink(realStore, link); err != nil {
		t.Skipf("symlinks not supported here: %v", err)
	}
	rep, err := Prune(link, Options{OlderThan: time.Hour, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	mustBeGone(t, old)
	if rep.Files != 1 {
		t.Errorf("Files = %d, want 1", rep.Files)
	}
}

func TestRemoveMessage(t *testing.T) {
	store := t.TempDir()
	a := cached(t, store, chatA, "DEL", "photo.jpg")
	tmp := cached(t, store, chatA, "DEL", ".download-1")
	other := cached(t, store, chatA, "OTHER", "doc.pdf")
	single := cached(t, store, chatB, "ONLY", "v.ogg")
	for _, p := range []string{a, tmp, other, single} {
		writeFile(t, p, newTime)
	}

	n, err := RemoveMessage(store, chatA, "DEL")
	if err != nil || n != 2 {
		t.Fatalf("RemoveMessage = %d, %v; want 2, nil", n, err)
	}
	mustBeGone(t, a, tmp, filepath.Dir(a))
	mustExist(t, other, filepath.Join(store, chatA))

	if n, err := RemoveMessage(store, chatB, "ONLY"); err != nil || n != 1 {
		t.Fatalf("RemoveMessage = %d, %v; want 1, nil", n, err)
	}
	mustBeGone(t, filepath.Join(store, chatB))

	// Nothing cached: not an error.
	if n, err := RemoveMessage(store, chatA, "NEVER"); err != nil || n != 0 {
		t.Errorf("RemoveMessage of uncached = %d, %v", n, err)
	}
	if n, err := RemoveMessage(store, "nochat@s.whatsapp.net", "X"); err != nil || n != 0 {
		t.Errorf("RemoveMessage of uncached chat = %d, %v", n, err)
	}
	if _, err := RemoveMessage(store, "../escape@s.whatsapp.net", "X"); !errors.Is(err, ErrEscapesStore) {
		t.Errorf("RemoveMessage escape = %v, want ErrEscapesStore", err)
	}
}

func TestRemoveMessage_RefusesLinkedDirectory(t *testing.T) {
	store := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "secret.pdf")
	writeFile(t, victim, oldTime)
	if err := os.MkdirAll(filepath.Join(store, chatA), 0o700); err != nil {
		t.Fatal(err)
	}
	dir, _ := MessageDir(store, chatA, "LINKED")
	dirLink(t, outside, dir)
	if _, err := RemoveMessage(store, chatA, "LINKED"); err == nil {
		t.Error("RemoveMessage followed a linked message directory")
	}
	mustExist(t, victim)

	// Same for a linked chat directory.
	store2 := t.TempDir()
	outChat := filepath.Join(outside, "chat")
	victim2 := filepath.Join(outChat, hex.EncodeToString([]byte("M")), "x.pdf")
	writeFile(t, victim2, oldTime)
	dirLink(t, outChat, filepath.Join(store2, chatA))
	if _, err := RemoveMessage(store2, chatA, "M"); err == nil {
		t.Error("RemoveMessage followed a linked chat directory")
	}
	mustExist(t, victim2)
}
