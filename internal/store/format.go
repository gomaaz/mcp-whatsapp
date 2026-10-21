package store

import (
	"context"
	"strings"
	"time"
)

// FormatMessage renders a single message in the textual format expected by
// downstream Claude clients. Every line carries the message ID and chat JID so
// any message (text or media) can be passed on to get_message_context or
// download_media. The time is printed in UTC as RFC 3339 with a "Z" suffix,
// the same form list_messages accepts for after/before.
func (s *Store) FormatMessage(ctx context.Context, m Message, showChatInfo bool) string {
	var b strings.Builder

	b.WriteString("[")
	b.WriteString(m.Timestamp.UTC().Format(time.RFC3339))
	b.WriteString("] [message_id=")
	b.WriteString(m.ID)
	b.WriteString(" chat_jid=")
	b.WriteString(m.ChatJID)
	b.WriteString("] ")

	if showChatInfo && m.ChatName != "" {
		b.WriteString("Chat: ")
		b.WriteString(s.displayName(m.ChatJID, m.ChatName))
		b.WriteString(" ")
	}

	contentPrefix := ""
	if m.MediaType != "" {
		contentPrefix = "[" + m.MediaType + "] "
	}

	var senderName string
	if m.IsFromMe {
		senderName = "Me"
	} else {
		senderName = s.GetSenderName(ctx, m.Sender)
	}

	b.WriteString("From: ")
	b.WriteString(senderName)
	b.WriteString(": ")
	b.WriteString(contentPrefix)
	b.WriteString(m.Content)
	b.WriteString("\n")

	return b.String()
}

// FormatMessagesList renders a slice of messages. Returns the literal string
// "No messages to display." when the slice is empty.
func (s *Store) FormatMessagesList(ctx context.Context, msgs []Message, showChatInfo bool) string {
	if len(msgs) == 0 {
		return "No messages to display."
	}
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(s.FormatMessage(ctx, m, showChatInfo))
	}
	return b.String()
}
