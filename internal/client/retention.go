package client

import (
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/mediacache"
)

// PruneMedia deletes downloaded media files not fetched or used through
// download_media for longer than olderThan, then the cache directories left
// empty. Message rows and cached transcripts stay, so download_media fetches
// a pruned file again on demand (as long as WhatsApp still serves it). The
// media root is never touched, even when it lies inside the store. Downloads
// wait while the cache is pruned.
func (c *Client) PruneMedia(olderThan time.Duration, dryRun bool) (mediacache.Report, error) {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	return mediacache.Prune(c.store.Dir(), mediacache.Options{
		OlderThan: olderThan,
		DryRun:    dryRun,
		Skip:      []string{c.allowedMediaRoot},
	})
}

// removeMessageMedia deletes the cached file of a message its sender deleted
// for everyone. It runs from the event handler, so when a download holds the
// cache it finishes in the background instead of stalling event handling;
// Download refuses revoked messages meanwhile, unless KeepRevoked is set.
// Failures are logged.
func (c *Client) removeMessageMedia(chatJID, messageID string) {
	if c.mediaMu.TryLock() {
		defer c.mediaMu.Unlock()
		c.removeMessageMediaLocked(chatJID, messageID)
		return
	}
	go func() {
		c.mediaMu.Lock()
		defer c.mediaMu.Unlock()
		c.removeMessageMediaLocked(chatJID, messageID)
	}()
}

func (c *Client) removeMessageMediaLocked(chatJID, messageID string) {
	n, err := mediacache.RemoveMessage(c.store.Dir(), chatJID, messageID)
	if err != nil {
		c.log.Warnf("Failed to delete cached media of deleted message %s: %v", c.redactor.MsgID(messageID), err)
		return
	}
	if n > 0 {
		c.log.Infof("Deleted %d cached media file(s) of deleted message %s", n, c.redactor.MsgID(messageID))
	}
}
