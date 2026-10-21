package client

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"

	"github.com/sealjay/mcp-whatsapp/internal/mediacache"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

const retJID = "447700000099@s.whatsapp.net"

// seedDownloadable stores a media message whose download goes through a
// fake downloadMedia that counts its calls.
func seedDownloadable(t *testing.T, payload []byte) (*Client, *store.Store, *int32) {
	t.Helper()
	t.Chdir(t.TempDir())
	s, err := store.Open("./store")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.StoreChat(retJID, "test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if err := s.StoreMessage(context.Background(), store.Message{
		ID: "M-RET", ChatJID: retJID, Sender: retJID, Content: "x", Timestamp: time.Now().UTC(),
		MediaType: "image", Filename: "photo.jpg", URL: "https://example.invalid/media", DirectPath: "/media",
	}, []byte{1}, digest[:], []byte{2}, uint64(len(payload))); err != nil {
		t.Fatal(err)
	}
	c := newClientWithStore(t, s)
	mediaRoot, err := filepath.Abs(filepath.Join("store", "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	c.allowedMediaRoot = mediaRoot
	var calls int32
	c.downloadMedia = func(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return payload, nil
	}
	return c, s, &calls
}

// age sets path's modification time d into the past.
func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func writeCached(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// After retention deleted a cached file, download_media must fetch it again
// instead of returning the deleted path.
func TestDownload_RefetchesAfterPrune(t *testing.T) {
	payload := []byte("JPEG-BYTES")
	c, _, calls := seedDownloadable(t, payload)

	first := c.Download(context.Background(), "M-RET", retJID, "")
	if !first.Success || *calls != 1 {
		t.Fatalf("first download: %+v, calls=%d", first, *calls)
	}
	age(t, first.Path, 48*time.Hour)

	rep, err := c.PruneMedia(24*time.Hour, false)
	if err != nil || rep.Files != 1 {
		t.Fatalf("PruneMedia = %+v, %v; want 1 file", rep, err)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("cached file survived pruning: %v", err)
	}

	second := c.Download(context.Background(), "M-RET", retJID, "")
	if !second.Success {
		t.Fatalf("download after prune failed: %+v", second)
	}
	if *calls != 2 {
		t.Errorf("download after prune did not fetch again (calls=%d)", *calls)
	}
	got, err := os.ReadFile(second.Path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("re-downloaded file = %q, %v", got, err)
	}
}

// A cache hit counts as use: the file's age restarts, so retention does not
// delete a file that was just handed out.
func TestDownload_CacheHitRestartsRetention(t *testing.T) {
	c, _, calls := seedDownloadable(t, []byte("JPEG-BYTES"))
	first := c.Download(context.Background(), "M-RET", retJID, "")
	if !first.Success {
		t.Fatal(first.Message)
	}
	age(t, first.Path, 48*time.Hour)
	if hit := c.Download(context.Background(), "M-RET", retJID, ""); !hit.Success || *calls != 1 {
		t.Fatalf("cache hit: %+v, calls=%d", hit, *calls)
	}
	if rep, err := c.PruneMedia(24*time.Hour, false); err != nil || rep.Files != 0 {
		t.Fatalf("PruneMedia = %+v, %v; want nothing deleted", rep, err)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("recently used file was pruned: %v", err)
	}
}

// PruneMedia leaves the media root alone even when it sits inside the store
// and is named like a chat directory.
func TestPruneMedia_SkipsMediaRoot(t *testing.T) {
	c, _, _ := seedDownloadable(t, []byte("x"))
	root, err := filepath.Abs(filepath.Join("store", "out@media"))
	if err != nil {
		t.Fatal(err)
	}
	c.allowedMediaRoot = root
	kept := filepath.Join(root, "4142", "mine.jpg")
	writeCached(t, kept, "x")
	age(t, kept, 48*time.Hour)
	if _, err := c.PruneMedia(time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("file in media root deleted: %v", err)
	}
}

// A delete for everyone removes the file download_media cached for the
// message, unless KeepRevoked is set. Other messages' files stay.
func TestHandleMessage_RevokeDeletesCachedMedia(t *testing.T) {
	for _, keep := range []bool{false, true} {
		c, s := newChangeClient(t)
		c.keepRevoked = keep
		c.handleMessage(liveMessage(t, chgDM, chgDM, "pic-1", false, chgTime,
			&waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Foto"), MediaKey: []byte{7}}}))
		dir, err := mediacache.MessageDir(s.Dir(), chgDM, "pic-1")
		if err != nil {
			t.Fatal(err)
		}
		cachedFile := filepath.Join(dir, "image.jpg")
		writeCached(t, cachedFile, "JPEG")
		otherDir, _ := mediacache.MessageDir(s.Dir(), chgDM, "other")
		otherFile := filepath.Join(otherDir, "doc.pdf")
		writeCached(t, otherFile, "PDF")

		c.handleMessage(liveMessage(t, chgDM, chgDM, "rev-1", false, chgTime.Add(time.Minute), revokePayload("pic-1")))

		_, statErr := os.Stat(cachedFile)
		if keep && statErr != nil {
			t.Errorf("keep=true: cached file deleted: %v", statErr)
		}
		if !keep && !os.IsNotExist(statErr) {
			t.Errorf("keep=false: cached file of revoked message still on disk (err %v)", statErr)
		}
		if _, err := os.Stat(otherFile); err != nil {
			t.Errorf("keep=%v: other message's file deleted: %v", keep, err)
		}
	}
}

// A revoke from someone other than the author changes nothing, so the file
// stays as well.
func TestHandleMessage_ForeignRevokeKeepsCachedMedia(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleMessage(liveMessage(t, chgGroup, chgDM, "g-pic", false, chgTime,
		&waProto.Message{ImageMessage: &waProto.ImageMessage{MediaKey: []byte{7}}}))
	dir, _ := mediacache.MessageDir(s.Dir(), chgGroup, "g-pic")
	f := filepath.Join(dir, "image.jpg")
	writeCached(t, f, "JPEG")
	c.handleMessage(liveMessage(t, chgGroup, "447700000003@s.whatsapp.net", "admin-rev", false, chgTime.Add(time.Minute), revokePayload("g-pic")))
	if r := readChg(t, s, chgGroup, "g-pic"); r.revoked {
		t.Fatal("foreign revoke was applied")
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("file deleted by a revoke from someone else: %v", err)
	}
}

// A file cached before its message was revoked (for example because
// deleting it failed) is not handed out, and is removed on sight.
func TestDownload_RefusesRevokedMessage(t *testing.T) {
	revoke := func(t *testing.T, s *store.Store, keep bool) {
		t.Helper()
		res, err := s.ApplyRevoke(context.Background(), store.MessageChange{
			ChatJID: retJID, ID: "M-RET", ActorSenders: []string{retJID}, At: time.Now(),
		}, keep)
		if err != nil || res != store.ChangeApplied {
			t.Fatalf("ApplyRevoke = %v, %v", res, err)
		}
	}

	c, s, calls := seedDownloadable(t, []byte("JPEG-BYTES"))
	first := c.Download(context.Background(), "M-RET", retJID, "")
	if !first.Success {
		t.Fatal(first.Message)
	}
	// Revoke through the store only, so the file stays behind.
	revoke(t, s, false)
	got := c.Download(context.Background(), "M-RET", retJID, "")
	if got.Success {
		t.Fatalf("revoked message's media handed out: %+v", got)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Errorf("leftover file of revoked message not removed (err %v)", err)
	}
	if *calls != 1 {
		t.Errorf("revoked message was fetched again (calls=%d)", *calls)
	}

	// With KeepRevoked the cached copy stays readable.
	c2, s2, _ := seedDownloadable(t, []byte("JPEG-BYTES"))
	c2.keepRevoked = true
	if dl := c2.Download(context.Background(), "M-RET", retJID, ""); !dl.Success {
		t.Fatal(dl.Message)
	}
	revoke(t, s2, true)
	if dl := c2.Download(context.Background(), "M-RET", retJID, ""); !dl.Success {
		t.Errorf("keep-revoked: cached media refused: %s", dl.Message)
	}
}
