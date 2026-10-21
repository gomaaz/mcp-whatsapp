package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// listMessageColumns is the 24-column projection shared by ListMessages,
// messageContext and queryContextWindow, scanned by scanListMessageRow. It
// must be selected FROM listMessageFrom, which provides the chats and quoted
// aliases. The quoted.* columns describe the cached original of a reply and
// are NULL when it is not a reply or the original is not cached.
const listMessageColumns = "messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type, messages.rowid, " +
	"messages.quoted_message_id, messages.quoted_sender, messages.quoted_text, messages.is_forwarded, " +
	"quoted.content, quoted.media_type, quoted.filename, quoted.sender, quoted.is_from_me, quoted.revoked_at, " +
	"messages.edited_at, messages.revoked_at, " +
	"messages.audio_seconds, messages.is_voice_note, messages.transcript"

// listMessageFrom joins each message to its chat and, for replies, to the
// quoted original in the same chat when it is cached.
const listMessageFrom = `messages
	JOIN chats ON messages.chat_jid = chats.jid
	LEFT JOIN messages AS quoted ON quoted.chat_jid = messages.chat_jid AND quoted.id = messages.quoted_message_id`

// MediaTypeNone is the MediaType filter value that selects text-only messages
// (no media attachment).
const MediaTypeNone = "none"

// ListMessagesParams holds the filters accepted by ListMessages. Zero values
// mean "unset" except for Limit, which defaults to 20 when <= 0.
type ListMessagesParams struct {
	// After and Before bound the message time as a half-open range: After is
	// inclusive, Before exclusive. Both are compared in UTC whatever their
	// location.
	After       time.Time
	Before      time.Time
	SenderPhone string
	ChatJID     string
	Query       string
	// FromMe restricts direction when non-nil: true keeps only own messages,
	// false only incoming ones.
	FromMe *bool
	// MediaType keeps only messages with this media type ("image", "video",
	// "audio", "document", "sticker"); MediaTypeNone keeps only text
	// messages. FromMe and MediaType select the matches only; the context
	// windows added by IncludeContext are not filtered.
	MediaType      string
	ExcludeGroups  bool
	Limit          int
	Page           int
	IncludeContext bool
	ContextBefore  int
	ContextAfter   int
}

// ListMessages returns messages matching the given filters, optionally expanded
// with surrounding context messages when IncludeContext is true.
func (s *Store) ListMessages(ctx context.Context, params ListMessagesParams) ([]Message, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	page := params.Page
	if page < 0 {
		page = 0
	}

	var (
		whereClauses []string
		args         []any
	)

	if !params.After.IsZero() {
		whereClauses = append(whereClauses, "messages.timestamp >= ?")
		args = append(args, dbTime(params.After))
	}
	if !params.Before.IsZero() {
		whereClauses = append(whereClauses, "messages.timestamp < ?")
		args = append(args, dbTime(params.Before))
	}
	if params.SenderPhone != "" {
		whereClauses = append(whereClauses, "messages.sender = ?")
		args = append(args, params.SenderPhone)
	}
	if params.ChatJID != "" {
		whereClauses = append(whereClauses, "messages.chat_jid = ?")
		args = append(args, params.ChatJID)
	}
	if params.Query != "" {
		// Cached voice-note transcripts are searched like message text.
		pattern := "%" + params.Query + "%"
		whereClauses = append(whereClauses, "(LOWER(messages.content) LIKE LOWER(?) OR LOWER(messages.transcript) LIKE LOWER(?))")
		args = append(args, pattern, pattern)
	}
	if params.FromMe != nil {
		whereClauses = append(whereClauses, "messages.is_from_me = ?")
		args = append(args, *params.FromMe)
	}
	switch params.MediaType {
	case "":
	case MediaTypeNone:
		whereClauses = append(whereClauses, "COALESCE(messages.media_type, '') = ''")
	default:
		whereClauses = append(whereClauses, "messages.media_type = ?")
		args = append(args, params.MediaType)
	}
	if params.ExcludeGroups {
		whereClauses = append(whereClauses, "messages.chat_jid NOT LIKE '%@g.us'")
	}

	var b strings.Builder
	b.WriteString("SELECT " + listMessageColumns + " FROM " + listMessageFrom)
	if len(whereClauses) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(whereClauses, " AND "))
	}
	b.WriteString(" ORDER BY messages.timestamp DESC")
	b.WriteString(" LIMIT ? OFFSET ?")

	offset := page * limit
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var result []Message
	for rows.Next() {
		m, err := s.scanListMessageRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if !params.IncludeContext || len(result) == 0 {
		if err := s.attachReactions(ctx, result); err != nil {
			return nil, err
		}
		return result, nil
	}

	before := params.ContextBefore
	if before <= 0 {
		before = 1
	}
	after := params.ContextAfter
	if after <= 0 {
		after = 1
	}

	// Expand context around each match. Windows of nearby matches overlap, so
	// keep each (chat_jid, id) once and return the lot in chronological order,
	// with rowid breaking ties between messages from the same second.
	var expanded []Message
	seen := make(map[string]bool)
	add := func(msgs ...Message) {
		for _, m := range msgs {
			key := m.ChatJID + "\x00" + m.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			expanded = append(expanded, m)
		}
	}
	for _, m := range result {
		c, err := s.messageContext(ctx, "messages.rowid = ?", m.rowID, before, after)
		if err != nil {
			return nil, fmt.Errorf("list messages context: %w", err)
		}
		add(c.Before...)
		add(c.Message)
		add(c.After...)
	}
	sort.SliceStable(expanded, func(i, j int) bool {
		a, b := expanded[i], expanded[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		return a.rowID < b.rowID
	})
	if err := s.attachReactions(ctx, expanded); err != nil {
		return nil, err
	}
	return expanded, nil
}

// ListChats returns one page of the chats matching query. sortBy is
// "last_active" (default) or "name". The query matches the stored chat name,
// the JID, the phone number behind an @lid chat and every address-book name
// (full, push, first or business name) of the chat's contact, also for chats
// stored under their @lid JID. Address-book names live in the separate
// whatsmeow DB, and "name" sorts by the resolved display name, so the match
// set is built, sorted and paged here; only the page is loaded in full.
func (s *Store) ListChats(ctx context.Context, query string, limit, page int, includeLastMessage bool, sortBy string) ([]Chat, error) {
	if limit <= 0 {
		limit = 20
	}
	if page < 0 {
		page = 0
	}

	type candidate struct {
		jid, name string
		resolved  bool // name is already the display name
	}
	rows, err := s.db.QueryContext(ctx, "SELECT jid, name FROM chats ORDER BY last_message_time DESC, jid")
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	var all []candidate
	for rows.Next() {
		var c candidate
		var name sql.NullString
		if err := rows.Scan(&c.jid, &name); err != nil {
			rows.Close()
			return nil, err
		}
		c.name = name.String
		all = append(all, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	cands := all
	if query != "" {
		contacts := s.searchContactNames("%"+query+"%", 0)
		needle := strings.ToLower(query)
		var matched []candidate
		for _, c := range all {
			if s.chatMatches(c.jid, c.name, needle, contacts) {
				matched = append(matched, c)
			}
		}
		cands = matched
	}

	if sortBy == "name" {
		for i := range cands {
			cands[i].name = s.displayName(cands[i].jid, cands[i].name)
			cands[i].resolved = true
		}
		sort.SliceStable(cands, func(i, j int) bool {
			a, b := strings.ToLower(cands[i].name), strings.ToLower(cands[j].name)
			if a != b {
				return a < b
			}
			return cands[i].jid < cands[j].jid
		})
	}

	start := page * limit
	if start >= len(cands) {
		return nil, nil
	}
	pageCands := cands[start:min(start+limit, len(cands))]

	args := make([]any, len(pageCands))
	for i, c := range pageCands {
		args[i] = c.jid
	}
	q := chatSelectSQL("chats", includeLastMessage) +
		" WHERE chats.jid IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	pageRows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer pageRows.Close()
	byJID := make(map[string]Chat, len(pageCands))
	for pageRows.Next() {
		c, err := scanChatRow(pageRows)
		if err != nil {
			return nil, err
		}
		byJID[c.JID] = c
	}
	if err := pageRows.Err(); err != nil {
		return nil, err
	}

	var result []Chat
	for _, pc := range pageCands {
		c, ok := byJID[pc.jid]
		if !ok {
			continue // removed since the first query
		}
		if pc.resolved {
			c.Name = pc.name
		} else {
			c.Name = s.displayName(c.JID, c.Name)
		}
		c.PhoneNumber = s.ResolveJIDToPhone(c.JID)
		result = append(result, c)
	}
	return result, nil
}

// chatMatches reports whether a chat matches a list_chats query. needle is
// the lower-cased query; contacts holds the address-book matches by phone
// JID. An @lid chat is compared through the phone JID it maps to.
func (s *Store) chatMatches(jid, stored, needle string, contacts map[string]string) bool {
	if strings.Contains(strings.ToLower(stored), needle) || strings.Contains(strings.ToLower(jid), needle) {
		return true
	}
	if strings.HasSuffix(jid, "@g.us") {
		return false
	}
	phoneJID := s.ResolveLIDToJID(jid)
	if phoneJID != jid && strings.Contains(phoneJID, needle) {
		return true
	}
	_, ok := contacts[phoneJID]
	return ok
}

// GetChat returns a single chat by JID. Returns (nil, nil) when not found.
func (s *Store) GetChat(ctx context.Context, jid string, includeLastMessage bool) (*Chat, error) {
	var b strings.Builder
	b.WriteString(chatSelectSQL("c", includeLastMessage))
	b.WriteString(" WHERE c.jid = ?")

	row := s.db.QueryRowContext(ctx, b.String(), jid)
	c, err := scanChatRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get chat: %w", err)
	}
	c.Name = s.displayName(c.JID, c.Name)
	c.PhoneNumber = s.ResolveJIDToPhone(c.JID)
	return &c, nil
}

// SearchContacts does a LIKE search over chats (excluding groups) and the
// synced whatsmeow_contacts address book, and returns up to 50 contacts
// ordered by name, jid. Address-book names win over chat names, which for
// 1:1 chats are often just the bare phone number.
func (s *Store) SearchContacts(ctx context.Context, query string) ([]Contact, error) {
	pattern := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT jid, name
		FROM chats
		WHERE (LOWER(name) LIKE LOWER(?) OR LOWER(jid) LIKE LOWER(?))
		  AND jid NOT LIKE '%@g.us'
		  AND jid NOT LIKE '%@lid'
		ORDER BY name, jid
		LIMIT 50`, pattern, pattern)
	if err != nil {
		return nil, fmt.Errorf("search contacts: %w", err)
	}
	defer rows.Close()

	var result []Contact
	for rows.Next() {
		var jid string
		var name sql.NullString
		if err := rows.Scan(&jid, &name); err != nil {
			return nil, err
		}
		result = append(result, Contact{
			PhoneNumber: jidUser(jid),
			Name:        name.String,
			JID:         jid,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Merge in address-book matches (issue #21: saved names live in
	// whatsmeow_contacts, not chats).
	names := s.searchContactNames(pattern, 50)
	seen := make(map[string]bool, len(result))
	for i, c := range result {
		seen[c.JID] = true
		if n := names[c.JID]; n != "" {
			result[i].Name = n
		}
	}
	for jid, name := range names {
		if seen[jid] {
			continue
		}
		result = append(result, Contact{PhoneNumber: jidUser(jid), Name: name, JID: jid})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].JID < result[j].JID
	})
	if len(result) > 50 {
		result = result[:50]
	}
	return result, nil
}

// GetMessageContext returns the target message together with before/after
// windows from the same chat, each with its current reactions.
func (s *Store) GetMessageContext(ctx context.Context, messageID string, before, after int) (*MessageContext, error) {
	mc, err := s.messageContext(ctx, "messages.id = ?", messageID, before, after)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("message with ID %s not found", messageID)
	}
	if err != nil {
		return nil, err
	}
	all := make([]Message, 0, len(mc.Before)+1+len(mc.After))
	all = append(append(append(all, mc.Before...), mc.Message), mc.After...)
	if err := s.attachReactions(ctx, all); err != nil {
		return nil, err
	}
	copy(mc.Before, all[:len(mc.Before)])
	mc.Message = all[len(mc.Before)]
	copy(mc.After, all[len(mc.Before)+1:])
	return mc, nil
}

// messageContext loads the message selected by where (a fixed predicate on
// the messages table with one placeholder) and the windows around it. It
// returns sql.ErrNoRows unwrapped when nothing matches.
func (s *Store) messageContext(ctx context.Context, where string, arg any, before, after int) (*MessageContext, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+listMessageColumns+`
		FROM `+listMessageFrom+`
		WHERE `+where, arg)

	target, err := s.scanListMessageRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("get message context: %w", err)
	}
	target.ChatName = s.displayName(target.ChatJID, target.ChatName)

	beforeMsgs, err := s.queryContextWindow(ctx, target.ChatJID, target.rowID, before, false)
	if err != nil {
		return nil, err
	}
	afterMsgs, err := s.queryContextWindow(ctx, target.ChatJID, target.rowID, after, true)
	if err != nil {
		return nil, err
	}
	// The window is the same chat, so reuse the resolved name.
	for i := range beforeMsgs {
		beforeMsgs[i].ChatName = target.ChatName
	}
	for i := range afterMsgs {
		afterMsgs[i].ChatName = target.ChatName
	}

	return &MessageContext{
		Message: target,
		Before:  beforeMsgs,
		After:   afterMsgs,
	}, nil
}

// GetSenderName resolves a sender JID to a display name, preferring the
// synced address book (whatsmeow_contacts) over the chats table — 1:1 chat
// names are often just the bare phone number. Falls back to the input value
// if no name is found.
func (s *Store) GetSenderName(ctx context.Context, senderJID string) string {
	if name := s.ContactName(senderJID); name != "" {
		return name
	}
	if name := s.chatNameByJID(ctx, senderJID); name != "" {
		return name
	}

	phonePart := jidUser(senderJID)

	// Prefer an exact direct-chat match on phone + @s.whatsapp.net before
	// falling back to a LIKE — the LIKE can cross-match unrelated rows.
	if name := s.chatNameByJID(ctx, phonePart+"@s.whatsapp.net"); name != "" {
		return name
	}

	var name2 sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT name FROM chats WHERE jid LIKE ? LIMIT 1", "%"+phonePart+"%").Scan(&name2)
	if err == nil && name2.Valid && name2.String != "" {
		return name2.String
	}
	return senderJID
}

// chatNameByJID looks up chats.name for an exact jid match. Returns "" if
// not found or the stored name is empty.
func (s *Store) chatNameByJID(ctx context.Context, jid string) string {
	var name sql.NullString
	if err := s.db.QueryRowContext(ctx,
		"SELECT name FROM chats WHERE jid = ? LIMIT 1", jid).Scan(&name); err == nil {
		return name.String
	}
	return ""
}

// queryContextWindow fetches `limit` messages in chat either before
// (ascending=false, newest first) or after (ascending=true, oldest first) the
// anchor row, ordered by (timestamp, rowid). The rowid tie-breaker keeps
// messages from the same second as the anchor in the window, and the anchor
// is compared against its own stored timestamp so it never lands in its own
// window.
func (s *Store) queryContextWindow(ctx context.Context, chatJID string, anchorRowID int64, limit int, ascending bool) ([]Message, error) {
	op := "<"
	order := "DESC"
	if ascending {
		op = ">"
		order = "ASC"
	}
	q := fmt.Sprintf(`
		SELECT `+listMessageColumns+`
		FROM `+listMessageFrom+`
		JOIN messages AS anchor ON anchor.rowid = ?
		WHERE messages.chat_jid = ?
			AND (messages.timestamp %[1]s anchor.timestamp
				OR (messages.timestamp = anchor.timestamp AND messages.rowid %[1]s anchor.rowid))
		ORDER BY messages.timestamp %[2]s, messages.rowid %[2]s
		LIMIT ?`, op, order)

	rows, err := s.db.QueryContext(ctx, q, anchorRowID, chatJID, limit)
	if err != nil {
		return nil, fmt.Errorf("context window: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := s.scanListMessageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// scanListMessageRow scans the 24-column listMessageColumns projection used
// by list_messages and the context window queries.
func (s *Store) scanListMessageRow(row interface {
	Scan(dest ...any) error
}) (Message, error) {
	var (
		ts            time.Time
		sender        string
		chatName      sql.NullString
		content       sql.NullString
		isFromMe      bool
		chatJID       string
		id            string
		mediaType     sql.NullString
		rowID         int64
		quotedID      sql.NullString
		quotedSender  sql.NullString
		quotedText    sql.NullString
		isForwarded   sql.NullBool
		origContent   sql.NullString
		origMediaType sql.NullString
		origFilename  sql.NullString
		origSender    sql.NullString
		origIsFromMe  sql.NullBool
		origRevoked   sql.NullTime
		editedAt      sql.NullTime
		revokedAt     sql.NullTime
		audioSeconds  sql.NullInt64
		isVoiceNote   sql.NullBool
		transcript    sql.NullString
	)
	if err := row.Scan(&ts, &sender, &chatName, &content, &isFromMe, &chatJID, &id, &mediaType, &rowID,
		&quotedID, &quotedSender, &quotedText, &isForwarded,
		&origContent, &origMediaType, &origFilename, &origSender, &origIsFromMe, &origRevoked,
		&editedAt, &revokedAt,
		&audioSeconds, &isVoiceNote, &transcript); err != nil {
		return Message{}, err
	}
	m := Message{
		ID:              id,
		ChatJID:         chatJID,
		Sender:          sender,
		Content:         content.String,
		Timestamp:       ts.UTC(),
		IsFromMe:        isFromMe,
		MediaType:       mediaType.String,
		ChatName:        chatName.String,
		QuotedMessageID: quotedID.String,
		QuotedSender:    quotedSender.String,
		QuotedText:      quotedText.String,
		IsForwarded:     isForwarded.Bool,
		IsVoiceNote:     isVoiceNote.Bool,
		Transcript:      transcript.String,
		rowID:           rowID,
	}
	if audioSeconds.Valid && audioSeconds.Int64 > 0 {
		m.AudioSeconds = uint32(audioSeconds.Int64)
	}
	if editedAt.Valid {
		t := editedAt.Time.UTC()
		m.EditedAt = &t
	}
	if revokedAt.Valid {
		t := revokedAt.Time.UTC()
		m.RevokedAt = &t
	}
	// The original is cached (quoted.is_from_me is NOT NULL for any stored
	// row): prefer its text and author over what the reply embedded.
	if m.QuotedMessageID != "" && origIsFromMe.Valid {
		snippet := QuoteSnippet(origContent.String, origMediaType.String, origFilename.String)
		switch {
		case origRevoked.Valid:
			// Its sender deleted it. The reply's embedded copy is the text
			// as it was before, so it is never shown; what the stored row
			// still holds (a media marker, or the text with -keep-revoked)
			// follows the marker.
			m.QuotedText = strings.TrimSpace(revokedMarker + " " + snippet)
		case snippet != "":
			m.QuotedText = snippet
		}
		m.QuotedFromMe = origIsFromMe.Bool
		if m.QuotedSender == "" {
			m.QuotedSender = origSender.String
		}
	}
	m.IsGroup = strings.HasSuffix(m.ChatJID, "@g.us")
	m.SenderPhone = s.resolveSenderPhone(m.Sender)
	if m.QuotedSender != "" {
		m.QuotedSender = s.resolveSenderPhone(m.QuotedSender)
	}
	return m, nil
}

// resolveSenderPhone returns a full phone (bare user part) for a message
// sender. The stored sender may be a LID user part (group participants, and
// DMs whose LID mapping was unknown when the message arrived), so try the
// LID map; a phone-number sender simply misses and is returned unchanged.
func (s *Store) resolveSenderPhone(sender string) string {
	if sender == "" || sender == "me" {
		return sender
	}
	resolved := s.ResolveLIDToJID(sender + "@lid")
	if u := jidUser(resolved); u != "" {
		return u
	}
	return sender
}

// chatSelectSQL returns the shared 6-column chat projection (jid, name,
// last_message_time, plus the latest message's content/sender/is_from_me
// when includeLastMessage) used by both ListChats and GetChat. alias
// qualifies the chat column references; pass "chats" for an unaliased FROM
// clause (ListChats) or a short alias like "c" when the caller appends its
// own WHERE clause on that alias (GetChat).
func chatSelectSQL(alias string, includeLastMessage bool) string {
	from := "chats"
	if alias != "chats" {
		from = "chats " + alias
	}
	if !includeLastMessage {
		return fmt.Sprintf(`SELECT
		%s.jid,
		%s.name,
		%s.last_message_time,
		NULL,
		NULL,
		NULL
	FROM %s`, alias, alias, alias, from)
	}
	return fmt.Sprintf(`SELECT
		%s.jid,
		%s.name,
		%s.last_message_time,
		latest_msg.content,
		latest_msg.sender,
		latest_msg.is_from_me
	FROM %s
		LEFT JOIN messages AS latest_msg ON %s.jid = latest_msg.chat_jid
		AND latest_msg.rowid = (
			SELECT rowid FROM messages
			WHERE chat_jid = %s.jid
			ORDER BY timestamp DESC LIMIT 1
		)`, alias, alias, alias, from, alias, alias)
}

// scanChatRow scans the 6-column projection used by ListChats and GetChat.
func scanChatRow(row interface {
	Scan(dest ...any) error
}) (Chat, error) {
	var (
		jid          string
		name         sql.NullString
		lastTime     sql.NullTime
		lastMessage  sql.NullString
		lastSender   sql.NullString
		lastIsFromMe sql.NullBool
	)
	if err := row.Scan(&jid, &name, &lastTime, &lastMessage, &lastSender, &lastIsFromMe); err != nil {
		return Chat{}, err
	}
	c := Chat{
		JID:          jid,
		Name:         name.String,
		LastMessage:  lastMessage.String,
		LastSender:   lastSender.String,
		LastIsFromMe: lastIsFromMe.Bool,
	}
	if lastTime.Valid {
		c.LastMessageTime = lastTime.Time.UTC()
	}
	c.IsGroup = strings.HasSuffix(c.JID, "@g.us")
	return c, nil
}
