package client

import (
	"context"
	"strconv"
	"strings"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// placeholderContent returns the readable text stored for message types that
// carry no text of their own: locations, live locations, contact cards and
// contact lists. Returns "" for every other message.
//
//	[location] Office, Main St 1 (52.52,13.405) https://maps.google.com/?q=52.52,13.405
//	[live location] (52.52,13.405) https://maps.google.com/?q=52.52,13.405
//	[contact] Alice Example, TEL +49 170 1234567
//	[contacts: 2] Alice Example, Bob
func placeholderContent(msg *waProto.Message) string {
	switch {
	case msg.GetLocationMessage() != nil:
		loc := msg.GetLocationMessage()
		kind := "location"
		if loc.GetIsLive() {
			kind = "live location"
		}
		return locationText(kind, loc.GetDegreesLatitude(), loc.GetDegreesLongitude(), loc.GetName(), loc.GetAddress())
	case msg.GetLiveLocationMessage() != nil:
		ll := msg.GetLiveLocationMessage()
		return locationText("live location", ll.GetDegreesLatitude(), ll.GetDegreesLongitude(), ll.GetCaption())
	case msg.GetContactMessage() != nil:
		cm := msg.GetContactMessage()
		return strings.TrimSpace("[contact] " + contactSummary(cm.GetDisplayName(), cm.GetVcard()))
	case msg.GetContactsArrayMessage() != nil:
		contacts := msg.GetContactsArrayMessage().GetContacts()
		names := make([]string, 0, len(contacts))
		for _, cm := range contacts {
			if name := contactName(cm.GetDisplayName(), cm.GetVcard()); name != "" {
				names = append(names, name)
			}
		}
		return strings.TrimSpace("[contacts: " + strconv.Itoa(len(contacts)) + "] " + strings.Join(names, ", "))
	}
	return ""
}

// locationText renders a location placeholder: the non-empty labels joined
// by ", ", then the coordinates and a map link. Coordinates of exactly 0,0
// are taken as missing.
func locationText(kind string, lat, lon float64, labels ...string) string {
	var parts []string
	for _, l := range labels {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			parts = append(parts, l)
		}
	}
	text := "[" + kind + "]"
	if len(parts) > 0 {
		text += " " + strings.Join(parts, ", ")
	}
	if lat != 0 || lon != 0 {
		coords := formatCoord(lat) + "," + formatCoord(lon)
		text += " (" + coords + ") https://maps.google.com/?q=" + coords
	}
	return text
}

// formatCoord prints a coordinate with at most six decimals (about 10 cm),
// without trailing zeros.
func formatCoord(v float64) string {
	s := strconv.FormatFloat(v, 'f', 6, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" {
		s = "0"
	}
	return s
}

// contactSummary describes a shared contact card: its name and phone
// numbers, e.g. "Alice Example, TEL +49 170 1234567, +49 30 123456".
func contactSummary(displayName, vcard string) string {
	name := contactName(displayName, vcard)
	_, tels := parseVCard(vcard)
	if len(tels) == 0 {
		return name
	}
	tel := "TEL " + strings.Join(tels, ", ")
	if name == "" {
		return tel
	}
	return name + ", " + tel
}

// contactName is the display name WhatsApp sent with a contact card, or the
// card's own FN/N name when that is empty.
func contactName(displayName, vcard string) string {
	if name := strings.Join(strings.Fields(displayName), " "); name != "" {
		return name
	}
	fn, _ := parseVCard(vcard)
	return fn
}

// handleUndecryptable stores a placeholder row for a message this device
// could not decrypt, so it does not silently go missing. whatsmeow asks the
// sender to send it again; a retry that decrypts arrives as a normal message
// with the same ID and replaces the placeholder (see handleMessage). Messages
// WhatsApp marks to be hidden on failure (DecryptFailHide) and status updates
// get no placeholder.
func (c *Client) handleUndecryptable(evt *events.UndecryptableMessage) {
	if evt.DecryptFailMode == events.DecryptFailHide || evt.Info.Chat.String() == types.StatusBroadcastJID.String() {
		c.log.Debugf("Undecryptable message %s in %s not stored", evt.Info.ID, c.redactor.JID(evt.Info.Chat.String()))
		return
	}
	c.log.Warnf("Undecryptable message %s from %s, storing a placeholder", evt.Info.ID, c.redactor.JID(evt.Info.Sender.String()))

	nm := c.normalizeIncomingMessage(
		evt.Info.Chat.String(),
		evt.Info.Sender.String(),
		evt.Info.Sender.User,
		evt.Info.Chat.Server == types.GroupServer,
		evt.Info.IsFromMe,
		"",
		evt.Info.ID,
		evt.Info.Timestamp,
		nil,
	)
	// The chat row goes first: messages.chat_jid references chats.jid.
	name := c.GetChatName(evt.Info.Chat, nm.chatJID, nil, nm.msg.Sender)
	if err := c.store.StoreChat(nm.chatJID, name, evt.Info.Timestamp); err != nil {
		c.log.Warnf("Failed to store chat: %v", err)
		return
	}
	if err := c.store.StoreUndecryptable(context.Background(), nm.msg); err != nil {
		c.log.Warnf("Failed to store undecryptable placeholder: %v", err)
	}
}
