package mcp

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// readOnlyTools is the allowlist registered in read-only mode: tools that only
// read the local cache or query WhatsApp without producing anything a contact
// or group can see. Deliberately absent: mark_read / mark_chat_read (they send
// read receipts) and get_group_invite_link (reset=true revokes the link).
var readOnlyTools = map[string]bool{
	"search_contacts":      true,
	"list_messages":        true,
	"list_chats":           true,
	"get_chat":             true,
	"get_message_context":  true,
	"download_media":       true,
	"request_sync":         true,
	"get_status":           true,
	"pairing_status":       true,
	"list_groups":          true,
	"get_group_info":       true,
	"get_blocklist":        true,
	"get_privacy_settings": true,
	"get_poll_results":     true,
	"is_on_whatsapp":       true,
}

// addTool registers tool unless the server is read-only and the tool is not on
// the readOnlyTools allowlist. Every registration goes through here, so a newly
// added tool stays hidden in read-only mode until it is allowlisted.
func (s *Server) addTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	if s.readOnly && !readOnlyTools[tool.Name] {
		return
	}
	s.mcp.AddTool(tool, handler)
}

// registerTools wires every MCP tool to the bound WhatsApp client.
//
// Tool registrations are split by domain:
//   - tools_query.go   — read-only data retrieval (list_chats, search_contacts, …)
//   - tools_send.go    — outbound messages (send_message, send_file, send_reaction, …)
//   - tools_message.go — message mutation & history (edit, delete, mark_read, sync, …)
//   - tools_groups.go  — group management (create, leave, participants, settings, …)
//   - tools_media.go   — polls + contact cards
//   - tools_privacy.go — blocklist, presence, privacy settings, status message
//
// Shared helpers live in tools_core.go (parseTimestamp, normalizeRecipientToChatJID,
// maybeMarkChatRead). Common result helpers live in result.go.
func (s *Server) registerTools() {
	s.registerQueryTools()
	s.registerSendTools()
	s.registerMessageTools()

	// Phase 1 — group management + blocklist.
	s.registerGroupTools()
	s.registerPrivacyTools()

	// Phase 2 — polls + contact cards.
	s.registerMediaTools()
}
