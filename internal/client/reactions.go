package client

import (
	"context"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// handleLiveReaction stores a live emoji reaction (or its removal) in the
// reactions table. The target is the message with the key's ID in the chat
// the reaction arrived in, never the key's RemoteJID.
func (c *Client) handleLiveReaction(msg *events.Message, nm normalizedMessage, rm *waProto.ReactionMessage) {
	c.storeReaction(context.Background(), reactionUpdate(nm.chatJID, nm, rm, msg.Info.Timestamp, c.ownUser()))
}

// reactionUpdate builds the stored form of a reaction message. Our own
// reactions are keyed by ownSender (our phone number) when it is known, so
// reactions from our different devices replace each other. The sender's own
// millisecond timestamp is preferred over the message time; both are capped
// at now.
func reactionUpdate(chatJID string, nm normalizedMessage, rm *waProto.ReactionMessage, ts time.Time, ownSender string) store.ReactionUpdate {
	sender := nm.msg.Sender
	if nm.msg.IsFromMe && ownSender != "" {
		sender = ownSender
	}
	if ms := rm.GetSenderTimestampMS(); ms > 0 {
		ts = time.UnixMilli(ms)
	}
	return store.ReactionUpdate{
		ChatJID:   chatJID,
		MessageID: rm.GetKey().GetID(),
		Sender:    sender,
		IsFromMe:  nm.msg.IsFromMe,
		Emoji:     rm.GetText(),
		Timestamp: clampToNow(ts),
	}
}

// ownReactionSender is the sender stored for our own reactions in a history
// sync: ownID when the payload named it, else our paired phone number.
func (c *Client) ownReactionSender(ownID string) string {
	if ownID != "" {
		return ownID
	}
	return c.ownUser()
}

// storeHistoryReactions stores the reactions a history-sync message carries
// with it (WebMessageInfo.Reactions). Each reaction's key names the person
// who reacted: our own when FromMe, else the participant (groups) or the
// other side of a direct chat.
func (c *Client) storeHistoryReactions(ctx context.Context, chatJID string, chat types.JID, ownID string, webMsg *waWeb.WebMessageInfo) {
	targetID := webMsg.GetKey().GetID()
	if targetID == "" {
		return
	}
	for _, r := range webMsg.GetReactions() {
		key := r.GetKey()
		u := store.ReactionUpdate{
			ChatJID:   chatJID,
			MessageID: targetID,
			Emoji:     r.GetText(),
			Timestamp: clampToNow(time.UnixMilli(r.GetSenderTimestampMS())),
		}
		switch {
		case key.GetFromMe():
			u.IsFromMe = true
			u.Sender = c.ownReactionSender(ownID)
		case key.GetParticipant() != "":
			u.Sender = c.quotedSenderUser(key.GetParticipant())
		case chat.Server != types.GroupServer:
			u.Sender = c.quotedSenderUser(chat.String())
		}
		if r.GetSenderTimestampMS() <= 0 {
			u.Timestamp = clampToNow(time.Unix(int64(webMsg.GetMessageTimestamp()), 0))
		}
		c.storeReaction(ctx, u)
	}
}

// storeReaction writes u and logs the outcome. A reaction without a target
// message ID or sender is skipped.
func (c *Client) storeReaction(ctx context.Context, u store.ReactionUpdate) {
	if u.MessageID == "" || u.Sender == "" {
		c.log.Debugf("Ignoring reaction without target or sender in %s", c.redactor.JID(u.ChatJID))
		return
	}
	if err := c.store.StoreReaction(ctx, u); err != nil {
		c.log.Warnf("Failed to store reaction to %s in %s: %v", c.redactor.MsgID(u.MessageID), c.redactor.JID(u.ChatJID), err)
		return
	}
	c.log.Debugf("Stored reaction to %s in %s", c.redactor.MsgID(u.MessageID), c.redactor.JID(u.ChatJID))
}
