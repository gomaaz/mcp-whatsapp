package client

import (
	"context"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// isChangeProtocol reports whether pm edits or revokes (deletes for
// everyone) an earlier message. REVOKE is the zero value of the type enum,
// so a nil pm must be ruled out first.
func isChangeProtocol(pm *waProto.ProtocolMessage) bool {
	if pm == nil {
		return false
	}
	switch pm.GetType() {
	case waProto.ProtocolMessage_MESSAGE_EDIT, waProto.ProtocolMessage_REVOKE:
		return true
	}
	return false
}

// handleLiveChange applies a live edit or revoke to the cached message it
// refers to. The target is looked up by the key's message ID in the chat the
// event arrived in (nm.chatJID), never by the key's RemoteJID, which may be a
// LID or name another chat. The store checks that the sender of the change
// wrote the target message.
func (c *Client) handleLiveChange(msg *events.Message, nm normalizedMessage, pm *waProto.ProtocolMessage) {
	ctx := context.Background()
	ch := store.MessageChange{
		ChatJID:      nm.chatJID,
		ID:           pm.GetKey().GetID(),
		ActorSenders: []string{nm.msg.Sender, msg.Info.Sender.User, msg.Info.SenderAlt.User},
		ActorFromMe:  msg.Info.IsFromMe,
		At:           clampToNow(msg.Info.Timestamp),
	}
	if ch.ID == "" {
		c.log.Debugf("Ignoring %s without a target message ID in %s", pm.GetType(), c.redactor.JID(ch.ChatJID))
		return
	}
	if pm.GetType() == waProto.ProtocolMessage_REVOKE {
		c.applyRevoke(ctx, ch)
		return
	}
	text, ok := editedText(pm.GetEditedMessage())
	if !ok {
		c.log.Debugf("Ignoring edit of %s in %s: no text in the edited message", c.redactor.MsgID(ch.ID), c.redactor.JID(ch.ChatJID))
		return
	}
	c.applyEdit(ctx, ch, text)
}

// editedText returns the new text of an edited message. An empty text is
// only accepted for image, video and document messages, where it means the
// caption was removed; for anything else an empty result means we could not
// read the edit, and blanking the stored text would be wrong.
func editedText(m *waProto.Message) (string, bool) {
	if m == nil {
		return "", false
	}
	if text := extractTextContent(m); text != "" {
		return text, true
	}
	if m.GetImageMessage() != nil || m.GetVideoMessage() != nil || m.GetDocumentMessage() != nil {
		return "", true
	}
	return "", false
}

// applyEdit stores an edit and logs the outcome. An edit that arrives before
// the message it edits finds nothing to update; that is logged at debug.
func (c *Client) applyEdit(ctx context.Context, ch store.MessageChange, text string) store.ChangeResult {
	res, err := c.store.ApplyEdit(ctx, ch, text)
	c.logChange("edit", ch, res, err)
	return res
}

// applyRevoke stores a delete-for-everyone and logs the outcome. Unless the
// client runs with KeepRevoked, the message's text and media keys are
// blanked and a file download_media already cached for it is deleted. A
// revoke whose message is not cached yet is kept as pending and applied when
// the message arrives (see applyPendingRevokes).
func (c *Client) applyRevoke(ctx context.Context, ch store.MessageChange) store.ChangeResult {
	res, err := c.store.ApplyRevoke(ctx, ch, c.keepRevoked)
	c.logChange("revoke", ch, res, err)
	switch {
	case err != nil:
	case res == store.ChangeApplied && !c.keepRevoked:
		c.removeMessageMedia(ch.ChatJID, ch.ID)
	case res == store.ChangeTargetMissing:
		if err := c.store.AddPendingRevoke(ctx, ch); err != nil {
			c.log.Warnf("Failed to keep revoke of %s in %s for later: %v", c.redactor.MsgID(ch.ID), c.redactor.JID(ch.ChatJID), err)
		}
	}
	return res
}

// applyPendingRevokes applies revokes that arrived before the message they
// delete, once that message is stored: the one for messageID, or every
// pending revoke of the chat when messageID is "".
func (c *Client) applyPendingRevokes(ctx context.Context, chatJID, messageID string) {
	applied, err := c.store.ApplyPendingRevokes(ctx, chatJID, messageID, c.keepRevoked)
	if err != nil {
		c.log.Warnf("Failed to apply pending revokes in %s: %v", c.redactor.JID(chatJID), err)
	}
	for _, ch := range applied {
		c.log.Infof("Applied pending revoke of %s in %s", c.redactor.MsgID(ch.ID), c.redactor.JID(ch.ChatJID))
		if !c.keepRevoked {
			c.removeMessageMedia(ch.ChatJID, ch.ID)
		}
	}
}

func (c *Client) logChange(kind string, ch store.MessageChange, res store.ChangeResult, err error) {
	chat, id := c.redactor.JID(ch.ChatJID), c.redactor.MsgID(ch.ID)
	switch {
	case err != nil:
		c.log.Warnf("Failed to apply %s of %s in %s: %v", kind, id, chat, err)
	case res == store.ChangeApplied:
		c.log.Infof("Applied %s of %s in %s", kind, id, chat)
	case res == store.ChangeSenderMismatch:
		c.log.Infof("Ignoring %s of %s in %s: %s", kind, id, chat, res)
	default:
		c.log.Debugf("Ignoring %s of %s in %s: %s", kind, id, chat, res)
	}
}

// historyRevokeTarget returns the ID of the message a history-sync entry
// revokes, or "" when it is not a revoke. History carries a sender's delete
// either as a REVOKE protocol message or as a REVOKE stub in place of the
// deleted message itself. Admin revokes (ADMIN_REVOKE stubs) are not
// honoured, as on the live path.
func historyRevokeTarget(webMsg *waWeb.WebMessageInfo, hm historyMessage) string {
	if webMsg.GetMessageStubType() == waWeb.WebMessageInfo_REVOKE {
		return hm.id
	}
	if pm := hm.message.GetProtocolMessage(); pm != nil && pm.GetType() == waProto.ProtocolMessage_REVOKE {
		return pm.GetKey().GetID()
	}
	return ""
}

// historyChange builds the MessageChange for an edit or revoke found in a
// history sync of chatJID.
func historyChange(chatJID, targetID string, nm normalizedMessage, hm historyMessage) store.MessageChange {
	return store.MessageChange{
		ChatJID:      chatJID,
		ID:           targetID,
		ActorSenders: []string{nm.msg.Sender, hm.senderUser},
		ActorFromMe:  hm.isFromMe,
		At:           clampToNow(hm.timestamp),
	}
}

// storeHistoryEdit stores an edit found in a history sync. handleHistorySync
// calls it once the conversation's messages are stored, oldest edit first.
// When the edited message is cached it gets the new text through ApplyEdit
// (with the author check; an older edit than the stored one is ignored).
// When it is not cached, the edit is stored as the message itself with
// edited_at set; StoreMessage keeps that text when the original arrives
// later only if the original has the same author. Reports whether anything
// was stored.
func (c *Client) storeHistoryEdit(ctx context.Context, nm normalizedMessage, hm historyMessage) bool {
	ch := historyChange(nm.msg.ChatJID, nm.msg.ID, nm, hm)
	res, err := c.store.ApplyEdit(ctx, ch, nm.msg.Content)
	if err == nil && res == store.ChangeTargetMissing {
		editedAt := ch.At
		nm.msg.EditedAt = &editedAt
		if err := c.store.StoreMessage(ctx, nm.msg, nm.mediaKey, nm.fileSHA256, nm.fileEncSHA256, nm.fileLength); err != nil {
			c.log.Warnf("Failed to store history message: %v", err)
			return false
		}
		return true
	}
	c.logChange("history edit", ch, res, err)
	return err == nil && res == store.ChangeApplied
}

// clampToNow caps a peer-supplied timestamp at the current time, so a
// far-future value cannot end up as an edit or delete time.
func clampToNow(t time.Time) time.Time {
	if now := time.Now(); t.After(now) {
		return now
	}
	return t
}
