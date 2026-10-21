package store

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// maxQuoteRunes caps the length of a stored quote snippet.
const maxQuoteRunes = 200

// FormatMessage renders a single message in the textual format expected by
// downstream Claude clients. Every line carries the message ID and chat JID so
// any message (text or media) can be passed on to get_message_context or
// download_media. The time is printed in UTC as RFC 3339 with a "Z" suffix,
// the same form list_messages accepts for after/before. Messages the sender
// deleted for everyone are marked "[deleted by sender]", edited ones
// "[edited]".
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
	if m.RevokedAt != nil {
		b.WriteString("[deleted by sender]")
		if m.Content != "" || contentPrefix != "" || m.QuotedMessageID != "" || m.IsForwarded {
			b.WriteString(" ")
		}
	} else if m.EditedAt != nil {
		b.WriteString("[edited] ")
	}
	if m.IsForwarded {
		b.WriteString("[forwarded] ")
	}
	if m.QuotedMessageID != "" {
		b.WriteString(s.formatQuote(ctx, m))
		b.WriteString(" ")
	}
	b.WriteString(contentPrefix)
	b.WriteString(m.Content)
	b.WriteString("\n")

	return b.String()
}

// formatQuote renders the reply marker of a message, e.g.
// (reply to 3EB0ABC from Alice: "see you at 5"). The author and snippet parts
// are left out when unknown.
func (s *Store) formatQuote(ctx context.Context, m Message) string {
	var b strings.Builder
	b.WriteString("(reply to ")
	b.WriteString(m.QuotedMessageID)
	switch {
	case m.QuotedFromMe:
		b.WriteString(" from Me")
	case m.QuotedSender != "":
		b.WriteString(" from ")
		b.WriteString(s.GetSenderName(ctx, m.QuotedSender))
	}
	if m.QuotedText != "" {
		b.WriteString(": ")
		b.WriteString(strconv.Quote(m.QuotedText))
	}
	b.WriteString(")")
	return b.String()
}

// QuoteSnippet builds the short text kept for a quoted message: its text,
// prefixed by a media marker ("[image]", "[document: offer.pdf]") when it
// carries media, on one line and cut to maxQuoteRunes runes. Returns "" when
// there is neither text nor media.
func QuoteSnippet(text, mediaType, filename string) string {
	marker := ""
	switch {
	case mediaType == "document" && filename != "":
		marker = "[document: " + filename + "]"
	case mediaType != "":
		marker = "[" + mediaType + "]"
	}
	snippet := strings.Join(strings.Fields(marker+" "+text), " ")
	if r := []rune(snippet); len(r) > maxQuoteRunes {
		snippet = string(r[:maxQuoteRunes-1]) + "…"
	}
	return snippet
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
