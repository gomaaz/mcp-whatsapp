package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// -- helpers shared across domain files -------------------------------------

// maybeMarkChatRead acks recent incoming messages in the chat after a
// successful send. Errors are swallowed on purpose: the caller already got a
// success response for the send, and the side-effect is best-effort.
func (s *Server) maybeMarkChatRead(ctx context.Context, r client.SendResult, recipient string, enabled bool) {
	if !enabled || !r.Success {
		return
	}
	chatJID := normalizeRecipientToChatJID(recipient)
	if chatJID == "" {
		return
	}
	_, _ = s.client.MarkChatRead(ctx, chatJID, 50)
}

// normalizeRecipientToChatJID maps a recipient string (phone or JID) back to
// a chat JID suitable for MarkChatRead / store lookups. Mirrors
// client.NormalizeRecipient so a successful send (which accepted `+447…`) is
// followed by a chat-JID lookup against the same canonical form. Malformed
// input returns "" so the caller skips the side-effect.
func normalizeRecipientToChatJID(recipient string) string {
	canonical, err := client.NormalizeRecipient(recipient)
	if err != nil {
		return ""
	}
	if strings.Contains(canonical, "@") {
		return canonical
	}
	return canonical + "@s.whatsapp.net"
}

// parseTimestamp reads a user-supplied time and returns it in UTC. It accepts
// RFC 3339 with "Z" or an offset, the same without an offset or with a space
// instead of the "T" (both read as UTC), and a bare "YYYY-MM-DD", which means
// 00:00 UTC on that day.
func parseTimestamp(s string) (time.Time, error) {
	formats := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp format: %s", s)
}

// timeFilterDesc explains the accepted forms of a time filter argument.
const timeFilterDesc = "ISO-8601 time: RFC 3339 with `Z` or an offset (`2026-10-20T08:00:00+02:00`), without an offset it is read as UTC; a bare date `YYYY-MM-DD` means 00:00 UTC that day"
