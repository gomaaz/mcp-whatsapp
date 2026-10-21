package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/sealjay/mcp-whatsapp/internal/security"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// StartEventHandler registers the event handler on the underlying whatsmeow
// client. Must be called before Connect().
func (c *Client) StartEventHandler() {
	if c.handlerInstalled {
		return
	}
	c.handlerID = c.wa.AddEventHandler(c.handleEvent)
	c.handlerInstalled = true
}

// handleEvent dispatches one whatsmeow event. Connection events also update
// the local health bookkeeping reported by get_status.
func (c *Client) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		c.handleMessage(v)
	case *events.HistorySync:
		c.handleHistorySync(v)
	case *events.Connected:
		c.health.connected(time.Now())
		c.log.Infof("Connected to WhatsApp")
	case *events.Disconnected:
		c.health.disconnected(time.Now())
		c.log.Warnf("Disconnected from WhatsApp, reconnecting")
	case *events.LoggedOut:
		c.health.loggedOut(time.Now(), v)
		c.log.Warnf("Device logged out (%s), please scan QR code to log in again", v.Reason)
	case *events.TemporaryBan:
		c.health.temporaryBan(time.Now(), v)
		c.log.Warnf("%s", v)
	// whatsmeow ends the connection after these without an
	// events.Disconnected and does not reconnect on its own.
	case *events.StreamReplaced:
		c.health.disconnected(time.Now())
		c.log.Warnf("Session replaced by another client with the same login, not reconnecting")
	case *events.ClientOutdated:
		c.health.disconnected(time.Now())
		c.log.Warnf("WhatsApp rejected this client version as outdated, not reconnecting")
	case *events.ConnectFailure:
		c.health.disconnected(time.Now())
		c.log.Warnf("Connection to WhatsApp failed (%s), not reconnecting", v.Reason)
	case *events.CATRefreshError:
		c.health.disconnected(time.Now())
		c.log.Warnf("Failed to refresh the WhatsApp access token, not reconnecting: %v", v.Error)
	case *events.OfflineSyncPreview:
		c.log.Infof("Offline sync starting - %d messages, %d receipts pending", v.Messages, v.Receipts)
	case *events.OfflineSyncCompleted:
		c.log.Infof("Offline sync completed - %d messages synced", v.Count)
	case *events.Receipt:
		c.log.Debugf("Receipt for %d messages: %s", len(v.MessageIDs), v.Type)
	case *events.UndecryptableMessage:
		c.handleUndecryptable(v)
	default:
		c.log.Debugf("Unhandled event type: %T", v)
	}
}

// normalizedMessage holds the result of normalizeIncomingMessage: the
// store-ready Message plus the raw media crypto fields and the full
// post-LID-resolution sender JID (used for poll-vote keying).
type normalizedMessage struct {
	msg           store.Message
	mediaKey      []byte
	fileSHA256    []byte
	fileEncSHA256 []byte
	fileLength    uint64
	senderFullJID string // post-LID sender as "user@server"
	chatJID       string // post-LID-normalised chat JID
}

// normalizeIncomingMessage resolves sender and chat JIDs (LID → phone),
// extracts text/media content, and returns a store-ready normalizedMessage.
// It covers the logic shared between handleMessage and handleHistorySync.
//
// Parameters:
//   - rawChatJID: the raw chat JID string (may be @lid for DMs).
//   - rawSenderJID: the raw sender JID string (may be @lid).
//   - senderUser: the user-part of the sender JID (fallback when no
//     participant is available).
//   - isGroup: true when rawChatJID ends with @g.us.
//   - isFromMe: whether the message was sent by the local device.
//   - ownID: the local device's user-part (used when isFromMe is true and
//     no participant is set).
//   - msgID: the message ID.
//   - timestamp: the message timestamp.
//   - raw: the protobuf Message payload.
func (c *Client) normalizeIncomingMessage(
	rawChatJID, rawSenderJID, senderUser string,
	isGroup, isFromMe bool,
	ownID string,
	msgID string,
	timestamp time.Time,
	raw *waProto.Message,
) normalizedMessage {
	// Normalise chat JID: convert LID to standard JID for direct chats.
	chatJID := rawChatJID
	if !isGroup {
		chatJID = c.store.ResolveLIDToJID(rawChatJID)
		if chatJID != rawChatJID {
			c.log.Infof("Normalized chat JID: %s -> %s", c.redactor.JID(rawChatJID), c.redactor.JID(chatJID))
		}
	}

	// Normalise sender (may be in LID form inside groups).
	sender := senderUser
	normalizedSender := c.store.ResolveLIDToJID(rawSenderJID)
	if normalizedSender != rawSenderJID {
		sender = strings.Split(normalizedSender, "@")[0]
		c.log.Infof("Normalized sender: %s -> %s", c.redactor.JID(senderUser), c.redactor.JID(sender))
	}

	// If the message is from us and we have no better sender, use ownID.
	if isFromMe && sender == "" && ownID != "" {
		sender = ownID
	}

	content := extractTextContent(raw)
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(raw)

	m := store.Message{
		ID:         msgID,
		ChatJID:    chatJID,
		Sender:     sender,
		Content:    content,
		Timestamp:  timestamp,
		IsFromMe:   isFromMe,
		MediaType:  mediaType,
		Filename:   filename,
		URL:        url,
		DirectPath: directPath,
	}
	m.AudioSeconds, m.IsVoiceNote = extractAudioInfo(raw)
	if ci := contextInfoOf(raw); ci != nil {
		m.IsForwarded = ci.GetIsForwarded()
		if quotedID := ci.GetStanzaID(); quotedID != "" {
			m.QuotedMessageID = quotedID
			m.QuotedSender = c.quotedSenderUser(ci.GetParticipant())
			m.QuotedText = quotedSnippet(ci.GetQuotedMessage())
		}
	}

	return normalizedMessage{
		msg:           m,
		mediaKey:      mediaKey,
		fileSHA256:    fileSHA256,
		fileEncSHA256: fileEncSHA256,
		fileLength:    fileLength,
		senderFullJID: normalizedSender,
		chatJID:       chatJID,
	}
}

// handleMessage persists an incoming message and its chat metadata.
func (c *Client) handleMessage(msg *events.Message) {
	if !msg.Info.IsFromMe {
		c.health.messageReceived(time.Now())
	}

	nm := c.normalizeIncomingMessage(
		msg.Info.Chat.String(),
		msg.Info.Sender.String(),
		msg.Info.Sender.User,
		strings.HasSuffix(msg.Info.Chat.String(), "@g.us"),
		msg.Info.IsFromMe,
		"", // ownID not needed for live messages — sender is always set
		msg.Info.ID,
		msg.Info.Timestamp,
		msg.Message,
	)

	// The message decrypted: drop the placeholder an earlier failed delivery
	// of it may have left. A message stored below replaces it anyway, but one
	// that is not stored as a row (reaction, poll vote, edit) would not.
	if err := c.store.DeleteUndecryptable(context.Background(), nm.chatJID, msg.Info.ID); err != nil {
		c.log.Warnf("Failed to drop undecryptable placeholder: %v", err)
	}

	// Reaction — kept in its own table, never as a message row, and it does
	// not move the chat's last activity.
	if rm := msg.Message.GetReactionMessage(); rm != nil {
		c.handleLiveReaction(msg, nm, rm)
		return
	}

	name := c.GetChatName(msg.Info.Chat, nm.chatJID, nil, nm.msg.Sender)

	if err := c.store.StoreChat(nm.chatJID, name, msg.Info.Timestamp); err != nil {
		c.log.Warnf("Failed to store chat: %v", err)
	}

	// Poll vote — a signal-only event with no user-visible content. Decrypt,
	// tally into the local cache, and return. We pass the post-LID-resolution
	// full JID so StorePollVote's primary key never collides across servers.
	if msg.Message != nil && msg.Message.GetPollUpdateMessage() != nil {
		voterFullJID := nm.senderFullJID
		if voterFullJID == "" {
			voterFullJID = msg.Info.Sender.String()
		}
		c.handlePollVote(msg, nm.chatJID, voterFullJID)
		return
	}

	// Edit or delete-for-everyone of an earlier message: update that row
	// instead of storing the protocol message itself.
	if pm := msg.Message.GetProtocolMessage(); isChangeProtocol(pm) {
		c.handleLiveChange(msg, nm, pm)
		return
	}

	// Poll creation — store a "[poll] <question>" row for readability and
	// cache the option names so we can decode vote SHA-256 hashes later.
	var pollOptions []string
	if msg.Message != nil {
		if pc := msg.Message.GetPollCreationMessage(); pc != nil {
			pollOptions = extractPollOptionNames(pc)
			if nm.msg.Content == "" {
				nm.msg.Content = "[poll] " + pc.GetName()
			}
		}
	}

	if nm.msg.Content == "" && nm.msg.MediaType == "" {
		return
	}

	err := c.store.StoreMessage(context.Background(), nm.msg, nm.mediaKey, nm.fileSHA256, nm.fileEncSHA256, nm.fileLength)

	if err == nil && len(pollOptions) > 0 {
		if perr := c.store.StorePollMetadata(context.Background(), msg.Info.ID, nm.chatJID, pollOptions); perr != nil {
			c.log.Warnf("Failed to store poll metadata for %s: %v", c.redactor.MsgID(msg.Info.ID), perr)
		}
	}

	if err != nil {
		c.log.Warnf("Failed to store message: %v", err)
		return
	}

	direction := "<-"
	if msg.Info.IsFromMe {
		direction = "->"
	}
	ts := logTime(msg.Info.Timestamp)
	if nm.msg.MediaType != "" {
		c.log.Infof("[%s] %s %s: [%s: %s] %s", ts, direction, c.redactor.JID(nm.msg.Sender), nm.msg.MediaType, c.redactor.Name(nm.msg.Filename), c.redactor.Body(nm.msg.Content))
	} else if nm.msg.Content != "" {
		c.log.Infof("[%s] %s %s: %s", ts, direction, c.redactor.JID(nm.msg.Sender), c.redactor.Body(nm.msg.Content))
	}
}

// handleHistorySync processes a bulk history payload from WhatsApp.
func (c *Client) handleHistorySync(historySync *events.HistorySync) {
	syncType := "UNKNOWN"
	if historySync.Data.SyncType != nil {
		syncType = historySync.Data.SyncType.String()
	}
	c.log.Infof("Received history sync event (type: %s) with %d conversations",
		syncType, len(historySync.Data.Conversations))

	ctx := context.Background()
	syncedCount := 0

	ownID := ""
	if c.wa.Store.ID != nil {
		ownID = c.wa.Store.ID.User
	}

	for _, conversation := range historySync.Data.Conversations {
		// Edits and revokes are applied after the conversation's messages
		// are stored, so each one finds the message it changes wherever it
		// sits in the payload. Edits then run oldest first, each through the
		// author check in ApplyEdit.
		var (
			edits   []historyEdit
			revokes []store.MessageChange
		)

		if conversation.ID == nil {
			continue
		}
		rawChatJID := *conversation.ID

		jid, err := types.ParseJID(rawChatJID)
		if err != nil {
			c.log.Warnf("Failed to parse JID %s: %v", c.redactor.JID(rawChatJID), err)
			continue
		}

		isGroup := strings.HasSuffix(rawChatJID, "@g.us")

		// Resolve the chat JID once for the conversation header. We
		// normalise it here so GetChatName and StoreChat use the
		// phone-number-based JID.
		chatJID := rawChatJID
		if !isGroup {
			chatJID = c.store.ResolveLIDToJID(rawChatJID)
			if chatJID != rawChatJID {
				c.log.Infof("History sync: Normalized chat JID: %s -> %s", c.redactor.JID(rawChatJID), c.redactor.JID(chatJID))
			}
		}

		name := c.GetChatName(jid, chatJID, conversation, "")

		messages := conversation.Messages
		if len(messages) == 0 {
			continue
		}

		latestMsg := messages[0]
		if latestMsg == nil || latestMsg.Message == nil {
			continue
		}
		ts := latestMsg.Message.GetMessageTimestamp()
		if ts == 0 {
			continue
		}
		_ = c.store.StoreChat(chatJID, name, time.Unix(int64(ts), 0))

		// Store oldest first, so rowid (the tie-breaker between messages of
		// the same second in context windows and list order) follows the
		// order in which they were sent.
		for _, msg := range slices.Backward(messages) {
			if msg == nil || msg.Message == nil {
				continue
			}

			if msg.Message.GetMessageTimestamp() == 0 {
				continue
			}

			hm := c.parseHistoryMessage(jid, ownID, msg.Message)
			timestamp := hm.timestamp

			// Reactions to this message travel with it in history.
			c.storeHistoryReactions(ctx, chatJID, jid, ownID, msg.Message)

			nm := c.normalizeIncomingMessage(
				rawChatJID, hm.rawSenderJID, hm.senderUser,
				isGroup, hm.isFromMe, ownID,
				hm.id, timestamp, hm.message,
			)

			if rm := hm.message.GetReactionMessage(); rm != nil {
				c.storeReaction(ctx, reactionUpdate(chatJID, nm, rm, hm.timestamp, c.ownReactionSender(ownID)))
				continue
			}

			if target := historyRevokeTarget(msg.Message, hm); target != "" {
				revokes = append(revokes, historyChange(chatJID, target, nm, hm))
				continue
			}

			c.log.Infof("Message content: %s, Media Type: %s", c.redactor.Body(nm.msg.Content), nm.msg.MediaType)
			if nm.msg.Content == "" && nm.msg.MediaType == "" {
				continue
			}

			// Override chatJID to the already-resolved one from the
			// conversation header — avoids re-resolving per message.
			nm.msg.ChatJID = chatJID

			if hm.edited {
				edits = append(edits, historyEdit{nm: nm, hm: hm})
				continue
			}
			if err := c.store.StoreMessage(ctx, nm.msg, nm.mediaKey, nm.fileSHA256, nm.fileEncSHA256, nm.fileLength); err != nil {
				c.log.Warnf("Failed to store history message: %v", err)
				continue
			}
			syncedCount++
			if nm.msg.MediaType != "" {
				c.log.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
					logTime(timestamp), c.redactor.JID(nm.msg.Sender), c.redactor.JID(chatJID), nm.msg.MediaType, c.redactor.Name(nm.msg.Filename), c.redactor.Body(nm.msg.Content))
			} else {
				c.log.Infof("Stored message: [%s] %s -> %s: %s",
					logTime(timestamp), c.redactor.JID(nm.msg.Sender), c.redactor.JID(chatJID), c.redactor.Body(nm.msg.Content))
			}
		}

		sort.SliceStable(edits, func(i, j int) bool { return edits[i].hm.timestamp.Before(edits[j].hm.timestamp) })
		for _, e := range edits {
			if c.storeHistoryEdit(ctx, e.nm, e.hm) {
				syncedCount++
			}
		}
		for _, ch := range revokes {
			c.applyRevoke(ctx, ch)
		}
	}

	c.log.Infof("History sync complete. Stored %d messages.", syncedCount)
}

// historyEdit is an edit found in a history sync, held back until the
// conversation's messages are stored.
type historyEdit struct {
	nm normalizedMessage
	hm historyMessage
}

// historyMessage holds the fields of one history-sync message that
// normalizeIncomingMessage needs, with the payload already unwrapped.
type historyMessage struct {
	rawSenderJID string
	senderUser   string
	isFromMe     bool
	id           string
	timestamp    time.Time
	message      *waProto.Message
	// edited is set when the payload was an edit: id is then the edited
	// message's ID and message its new content.
	edited bool
}

// parseHistoryMessage reads one history-sync message through whatsmeow's
// ParseWebMessage, which unwraps ephemeral, view-once, document-with-caption,
// edited and device-sent wrappers the same way live messages are unwrapped.
// Without this, wrapped history messages arrive with no text or media and
// are dropped. If ParseWebMessage fails (not logged in, group message with
// no participant), it falls back to reading the message key directly.
func (c *Client) parseHistoryMessage(chat types.JID, ownID string, webMsg *waWeb.WebMessageInfo) historyMessage {
	evt, err := c.wa.ParseWebMessage(chat, webMsg)
	if err != nil {
		c.log.Debugf("History sync: ParseWebMessage failed for %s, reading the key directly: %v", c.redactor.MsgID(webMsg.GetKey().GetID()), err)
		return historyMessageFromKey(chat, ownID, webMsg)
	}
	hm := historyMessage{
		rawSenderJID: evt.Info.Sender.String(),
		senderUser:   evt.Info.Sender.User,
		isFromMe:     evt.Info.IsFromMe,
		id:           evt.Info.ID,
		timestamp:    evt.Info.Timestamp,
		message:      evt.Message,
		// ParseWebMessage has already swapped in the edited message's ID
		// and content.
		edited: unwrapHistoryPayload(webMsg).GetProtocolMessage().GetType() == waProto.ProtocolMessage_MESSAGE_EDIT,
	}
	// ParseWebMessage names our own LID as sender in @lid chats, which the
	// LID map may not resolve. Keep our own messages under our phone number.
	if hm.isFromMe && ownID != "" {
		hm.rawSenderJID = ownID + "@" + types.DefaultUserServer
		hm.senderUser = ownID
	}
	return hm
}

// historyMessageFromKey takes sender, direction and ID straight from the
// message key. The payload is still unwrapped so the text and media of
// wrapped messages survive, and an edit is turned into the edited message's
// ID and new content, as ParseWebMessage does.
func historyMessageFromKey(chat types.JID, ownID string, webMsg *waWeb.WebMessageInfo) historyMessage {
	hm := historyMessage{
		rawSenderJID: chat.String(),
		senderUser:   chat.User,
		id:           webMsg.GetKey().GetID(),
		timestamp:    time.Unix(int64(webMsg.GetMessageTimestamp()), 0),
		message:      unwrapHistoryPayload(webMsg),
	}
	if pm := hm.message.GetProtocolMessage(); pm.GetType() == waProto.ProtocolMessage_MESSAGE_EDIT {
		hm.id = pm.GetKey().GetID()
		hm.message = pm.GetEditedMessage()
		hm.edited = true
	}
	if key := webMsg.GetKey(); key != nil {
		hm.isFromMe = key.GetFromMe()
		if !hm.isFromMe && key.GetParticipant() != "" {
			hm.rawSenderJID = key.GetParticipant()
			hm.senderUser = strings.Split(hm.rawSenderJID, "@")[0]
		} else if hm.isFromMe {
			hm.rawSenderJID = ownID + "@s.whatsapp.net"
			hm.senderUser = ownID
		}
	}
	return hm
}

// unwrapHistoryPayload returns the message payload of a history-sync message
// with ephemeral, view-once, device-sent and similar wrappers removed.
func unwrapHistoryPayload(webMsg *waWeb.WebMessageInfo) *waProto.Message {
	return (&events.Message{RawMessage: webMsg.GetMessage()}).UnwrapRaw().Message
}

// GetChatName determines the appropriate name for a chat. It checks the
// existing database entry first, then falls back to conversation metadata
// (for history sync), contact store lookups, and finally the JID.
// A stored name that is only the chat's number or our own number is a
// placeholder and gets resolved again, so a later contact sync can fix it.
// The conversation parameter is the typed whatsmeow Conversation from a
// history-sync payload; pass nil for live messages.
func (c *Client) GetChatName(jid types.JID, chatJID string, conversation *waHistorySync.Conversation, sender string) string {
	chatUser, _, _ := strings.Cut(chatJID, "@")
	if existing := c.store.FindChatName(chatJID); existing != "" &&
		existing != chatUser && existing != jid.User && existing != c.ownUser() {
		c.log.Infof("Using existing chat name for %s: %s", c.redactor.JID(chatJID), c.redactor.Name(existing))
		return existing
	}

	var name string

	if jid.Server == "g.us" {
		c.log.Infof("Getting name for group: %s", c.redactor.JID(chatJID))

		if conversation != nil {
			if dn := conversation.GetDisplayName(); dn != "" {
				name = dn
			} else if cn := conversation.GetName(); cn != "" {
				name = cn
			}
		}

		if name == "" {
			name = c.resolveChatNameFallback(context.Background(), jid, sender)
		}

		c.log.Infof("Using group name: %s", c.redactor.Name(name))
	} else {
		c.log.Infof("Getting name for contact: %s", c.redactor.JID(chatJID))

		name = c.resolveChatNameFallback(context.Background(), jid, sender)

		c.log.Infof("Using contact name: %s", c.redactor.Name(name))
	}

	return name
}

// resolveChatNameFallback looks up a chat's name via GetGroupInfo (for
// groups) or the contact store (for DMs: full name, then push name, then
// business name), falling back to sender (if given and not ourselves) and
// finally jid.User. This is the shared tail of GetChatName's group and
// contact branches, also used directly by persistSent (which has no
// conversation metadata to check first).
func (c *Client) resolveChatNameFallback(ctx context.Context, jid types.JID, sender string) string {
	if jid.Server == "g.us" {
		if gi, err := c.wa.GetGroupInfo(ctx, jid); err == nil && gi.Name != "" {
			return gi.Name
		}
		return "Group " + jid.User
	}
	if contact, err := c.wa.Store.Contacts.GetContact(ctx, jid); err == nil {
		for _, name := range []string{contact.FullName, contact.PushName, contact.BusinessName} {
			if name != "" {
				return name
			}
		}
	}
	// In a DM we wrote first, sender is our own number, not the chat's name.
	if sender != "" && sender != c.ownUser() {
		return sender
	}
	return jid.User
}

// ownUser returns our own phone number (JID user part), or "" when the
// device is not paired.
func (c *Client) ownUser() string {
	if c.wa == nil || c.wa.Store == nil || c.wa.Store.ID == nil {
		return ""
	}
	return c.wa.Store.ID.User
}

// extractTextContent returns the plain-text body of a Message, if any.
// For image, video and document messages it returns the caption, since
// WhatsApp carries the accompanying text there instead of in Conversation.
// Locations and contact cards have no text of their own; they get a readable
// placeholder ("[location] …", "[contact] …", see placeholderContent) so they
// are stored instead of being dropped as empty.
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	if text := msg.GetConversation(); text != "" {
		return text
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}
	if ptv := msg.GetPtvMessage(); ptv != nil {
		return ptv.GetCaption()
	}
	return placeholderContent(msg)
}

// contextInfoOf returns the ContextInfo of the message types that can quote
// another message or carry the forwarded flag: extended text, image, video,
// video note, document, audio, sticker, location, live location and contact
// cards. Plain Conversation messages have none.
func contextInfoOf(msg *waProto.Message) *waProto.ContextInfo {
	if msg == nil {
		return nil
	}
	switch {
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetContextInfo()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetContextInfo()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetContextInfo()
	case msg.GetPtvMessage() != nil:
		return msg.GetPtvMessage().GetContextInfo()
	case msg.GetLocationMessage() != nil:
		return msg.GetLocationMessage().GetContextInfo()
	case msg.GetLiveLocationMessage() != nil:
		return msg.GetLiveLocationMessage().GetContextInfo()
	case msg.GetContactMessage() != nil:
		return msg.GetContactMessage().GetContextInfo()
	case msg.GetContactsArrayMessage() != nil:
		return msg.GetContactsArrayMessage().GetContextInfo()
	}
	return nil
}

// quotedSnippet summarises the copy of the quoted message that a reply
// carries: its text or caption, with a media marker for image, video, audio,
// document and sticker messages (see store.QuoteSnippet).
func quotedSnippet(quoted *waProto.Message) string {
	if quoted == nil {
		return ""
	}
	mediaType, filename := "", ""
	switch {
	case quoted.GetImageMessage() != nil:
		mediaType = "image"
	case quoted.GetVideoMessage() != nil, quoted.GetPtvMessage() != nil:
		mediaType = "video"
	case quoted.GetAudioMessage() != nil:
		mediaType = "audio"
	case quoted.GetDocumentMessage() != nil:
		mediaType = "document"
		filename = quoted.GetDocumentMessage().GetFileName()
	case quoted.GetStickerMessage() != nil:
		mediaType = "sticker"
	}
	return store.QuoteSnippet(extractTextContent(quoted), mediaType, filename)
}

// quotedSenderUser turns the participant JID of a quoted message into the
// bare phone/user part, resolving a LID to the phone number when the
// mapping is known. Returns "" when the participant is empty or unparsable.
func (c *Client) quotedSenderUser(participant string) string {
	if participant == "" {
		return ""
	}
	jid, err := types.ParseJID(participant)
	if err != nil || jid.User == "" {
		return ""
	}
	return strings.Split(c.store.ResolveLIDToJID(jid.ToNonAD().String()), "@")[0]
}

// extractMediaInfo pulls out the storable media fields from a Message.
// directPath is captured verbatim from the media protobuf and includes the
// signed `?ccb=&oh=&oe=&_nc_sid=` CDN auth query, so the download path can
// hand it straight to whatsmeow without any URL string manipulation.
// Stickers are stored as media type "sticker" (a WebP image), video notes
// (round PTV videos) as "video"; both download like any other media.
func extractMediaInfo(msg *waProto.Message) (mediaType, filename, url, directPath string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", "", nil, nil, nil, 0
	}

	if img := msg.GetImageMessage(); img != nil {
		return "image", generatedMediaFilename("image", img.GetFileSHA256(), ".jpg"),
			img.GetURL(), img.GetDirectPath(),
			img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", generatedMediaFilename("video", vid.GetFileSHA256(), ".mp4"),
			vid.GetURL(), vid.GetDirectPath(),
			vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", generatedMediaFilename("audio", aud.GetFileSHA256(), ".ogg"),
			aud.GetURL(), aud.GetDirectPath(),
			aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		fname := security.SafeFilename(doc.GetFileName())
		return "document", fname,
			doc.GetURL(), doc.GetDirectPath(),
			doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}
	if ptv := msg.GetPtvMessage(); ptv != nil {
		return "video", generatedMediaFilename("video", ptv.GetFileSHA256(), ".mp4"),
			ptv.GetURL(), ptv.GetDirectPath(),
			ptv.GetMediaKey(), ptv.GetFileSHA256(), ptv.GetFileEncSHA256(), ptv.GetFileLength()
	}
	if st := msg.GetStickerMessage(); st != nil {
		return "sticker", generatedMediaFilename("sticker", st.GetFileSHA256(), ".webp"),
			st.GetURL(), st.GetDirectPath(),
			st.GetMediaKey(), st.GetFileSHA256(), st.GetFileEncSHA256(), st.GetFileLength()
	}

	return "", "", "", "", nil, nil, nil, 0
}

// extractAudioInfo returns the duration WhatsApp reports for an audio message
// and whether it was recorded as a voice note (PTT). Both are zero for
// anything that is not audio.
func extractAudioInfo(msg *waProto.Message) (seconds uint32, isVoiceNote bool) {
	aud := msg.GetAudioMessage()
	if aud == nil {
		return 0, false
	}
	return aud.GetSeconds(), aud.GetPTT()
}

func generatedMediaFilename(prefix string, fileSHA256 []byte, extension string) string {
	digest := hex.EncodeToString(fileSHA256)
	if digest == "" {
		digest = "unknown"
	}
	return prefix + "_" + digest + extension
}

// extractPollOptionNames pulls the option names out of a PollCreationMessage.
// Returns nil if the input is nil or the options list is empty.
func extractPollOptionNames(pc *waProto.PollCreationMessage) []string {
	if pc == nil {
		return nil
	}
	opts := pc.GetOptions()
	if len(opts) == 0 {
		return nil
	}
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.GetOptionName())
	}
	return out
}

// handlePollVote decrypts an incoming PollUpdateMessage, reverses its
// SelectedOptions SHA-256 hashes against the cached option names, and records
// the voter's current selection. Failures are logged (without poll content)
// and swallowed so a broken vote never blocks the rest of message handling.
// voterJID must be the full post-LID-resolution JID string.
func (c *Client) handlePollVote(msg *events.Message, chatJID, voterJID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vote, err := c.wa.DecryptPollVote(ctx, msg)
	if err != nil {
		c.log.Warnf("Failed to decrypt poll vote from %s in %s: %v", c.redactor.JID(voterJID), c.redactor.JID(chatJID), err)
		return
	}

	pu := msg.Message.GetPollUpdateMessage()
	key := pu.GetPollCreationMessageKey()
	pollID := key.GetID()
	if pollID == "" {
		c.log.Warnf("Poll vote missing poll creation message id (chat=%s)", c.redactor.JID(chatJID))
		return
	}

	// Scope lookup to our locally-observed chatJID; ignore key.RemoteJID to
	// prevent a crafted update from another chat polluting this chat's tally.
	options, err := c.store.GetPollOptions(ctx, pollID, chatJID)
	if err != nil {
		// No cached metadata — we've probably never seen the poll creation
		// message. Log without listing option text and bail.
		c.log.Warnf("Poll vote for unknown poll %s in %s: %v", c.redactor.MsgID(pollID), c.redactor.JID(chatJID), err)
		return
	}

	optionHashes := whatsmeow.HashPollOptions(options)
	selectedHashes := vote.GetSelectedOptions()
	selected := make([]string, 0, len(selectedHashes))
	for _, h := range selectedHashes {
		for i, known := range optionHashes {
			if bytes.Equal(h, known) {
				selected = append(selected, options[i])
				break
			}
		}
	}

	// Clamp the peer-supplied timestamp to now so a malicious far-future value
	// can't permanently suppress later legitimate updates via the replay guard.
	ts := msg.Info.Timestamp
	if now := time.Now(); ts.After(now) {
		ts = now
	}

	if err := c.store.StorePollVote(ctx, pollID, chatJID, voterJID, selected, ts); err != nil {
		c.log.Warnf("Failed to store poll vote for poll %s in %s: %v", c.redactor.MsgID(pollID), c.redactor.JID(chatJID), err)
		return
	}

	c.log.Debugf("Recorded poll vote from %s on poll %s with %d selections", c.redactor.JID(voterJID), c.redactor.MsgID(pollID), len(selected))
}
