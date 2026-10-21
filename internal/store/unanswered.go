package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UnansweredChat is a chat whose newest incoming messages have no reply from
// us yet: every incoming message after our last own message (and inside the
// requested time window) counts as waiting. An own reaction to an incoming
// message counts as a reply to that message and everything before it.
type UnansweredChat struct {
	ChatJID     string `json:"chat_jid"`
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number,omitempty"`
	IsGroup     bool   `json:"is_group"`
	// WaitingCount is the number of incoming messages waiting for a reply.
	WaitingCount int `json:"waiting_count"`
	// FirstWaitingAt and LastWaitingAt are the times (UTC) of the oldest and
	// newest waiting message.
	FirstWaitingAt time.Time `json:"first_waiting_at"`
	LastWaitingAt  time.Time `json:"last_waiting_at"`
	// LastReplyAt is when we last wrote or reacted in the chat; nil if we
	// never did.
	LastReplyAt *time.Time `json:"last_reply_at,omitempty"`
	// LastMessage is the newest waiting message.
	LastMessage UnansweredMessage `json:"last_message"`
}

// UnansweredMessage summarises the newest waiting message of a chat. A text
// message carries its body in Text; an image, video or document carries its
// caption (if any) in Caption instead.
type UnansweredMessage struct {
	ID        string `json:"id"`
	Sender    string `json:"sender,omitempty"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Caption   string `json:"caption,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

// unansweredChatFilter excludes chats nobody expects a reply in: status
// updates (status@broadcast), broadcast lists (<id>@broadcast) and channels
// (<id>@newsletter).
const unansweredChatFilter = "messages.chat_jid NOT LIKE '%@broadcast' AND messages.chat_jid NOT LIKE '%@newsletter'"

// ListUnanswered returns the chats in which incoming messages are waiting for
// a reply: for each chat, last_out is the time of our newest own message, and
// every incoming message newer than both since and last_out is waiting.
// Every own message counts, placeholder rows such as stickers, locations and
// contact cards included. An own reaction (not a removed one) to a cached
// message moves last_out up to the time of the message reacted to, so that
// message and everything before it count as answered, while messages that
// came after it keep waiting. Messages deleted for everyone count on neither
// side, and rows without text or media (signal-only rows) are ignored. Group chats are skipped unless
// includeGroups is set. since is inclusive; a zero since means no lower bound.
// The result is sorted by the oldest waiting message first, so the longest
// wait comes first. limit defaults to 20 when <= 0.
func (s *Store) ListUnanswered(ctx context.Context, since time.Time, includeGroups bool, limit int) ([]UnansweredChat, error) {
	if limit <= 0 {
		limit = 20
	}

	where := []string{
		"messages.is_from_me = 0",
		"messages.revoked_at IS NULL",
		"(COALESCE(messages.content, '') <> '' OR COALESCE(messages.media_type, '') <> '')",
		"(last_out.t IS NULL OR messages.timestamp > last_out.t)",
		unansweredChatFilter,
	}
	var args []any
	if !since.IsZero() {
		where = append(where, "messages.timestamp >= ?")
		args = append(args, dbTime(since))
	}
	if !includeGroups {
		where = append(where, "messages.chat_jid NOT LIKE '%@g.us'")
	}
	args = append(args, limit)

	// Timestamps are uniform UTC text (see dbTime), so MAX/MIN and the
	// comparisons order them correctly. rowid breaks ties between waiting
	// messages from the same second when picking the newest one.
	q := `
		WITH answered AS (
			SELECT chat_jid, timestamp AS t, timestamp AS replied
			FROM messages
			WHERE is_from_me = 1 AND revoked_at IS NULL
			UNION ALL
			SELECT reactions.chat_jid, target.timestamp, reactions.timestamp
			FROM reactions
			JOIN messages AS target ON target.chat_jid = reactions.chat_jid AND target.id = reactions.message_id
			WHERE reactions.is_from_me = 1 AND reactions.emoji <> ''
		),
		last_out AS (
			SELECT chat_jid, MAX(t) AS t, MAX(replied) AS replied
			FROM answered
			GROUP BY chat_jid
		),
		waiting AS (
			SELECT messages.rowid AS rid, messages.chat_jid, messages.timestamp AS ts, last_out.replied AS last_out,
				ROW_NUMBER() OVER (PARTITION BY messages.chat_jid ORDER BY messages.timestamp DESC, messages.rowid DESC) AS rn,
				COUNT(*) OVER (PARTITION BY messages.chat_jid) AS cnt,
				MIN(messages.timestamp) OVER (PARTITION BY messages.chat_jid) AS first_ts
			FROM messages
			LEFT JOIN last_out ON last_out.chat_jid = messages.chat_jid
			WHERE ` + strings.Join(where, " AND ") + `
		)
		SELECT waiting.chat_jid, chats.name, waiting.cnt,
			CAST(waiting.first_ts AS TEXT), CAST(waiting.ts AS TEXT), CAST(waiting.last_out AS TEXT),
			m.id, m.sender, m.content, m.media_type, m.filename
		FROM waiting
		JOIN messages AS m ON m.rowid = waiting.rid
		LEFT JOIN chats ON chats.jid = waiting.chat_jid
		WHERE waiting.rn = 1
		ORDER BY waiting.first_ts ASC, waiting.chat_jid ASC
		LIMIT ?`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list unanswered: %w", err)
	}
	defer rows.Close()

	var result []UnansweredChat
	for rows.Next() {
		var (
			chatJID   string
			chatName  sql.NullString
			count     int
			firstTS   sql.NullString
			lastTS    sql.NullString
			lastOut   sql.NullString
			id        string
			sender    sql.NullString
			content   sql.NullString
			mediaType sql.NullString
			filename  sql.NullString
		)
		if err := rows.Scan(&chatJID, &chatName, &count, &firstTS, &lastTS, &lastOut,
			&id, &sender, &content, &mediaType, &filename); err != nil {
			return nil, fmt.Errorf("scan unanswered: %w", err)
		}
		c := UnansweredChat{
			ChatJID:      chatJID,
			Name:         s.displayName(chatJID, chatName.String),
			PhoneNumber:  s.ResolveJIDToPhone(chatJID),
			IsGroup:      strings.HasSuffix(chatJID, "@g.us"),
			WaitingCount: count,
			LastMessage: UnansweredMessage{
				ID:        id,
				Sender:    s.resolveSenderPhone(jidUser(sender.String)),
				MediaType: mediaType.String,
				Filename:  filename.String,
			},
		}
		if c.LastMessage.MediaType == "" {
			c.LastMessage.Text = content.String
		} else {
			c.LastMessage.Caption = content.String
		}
		if t, ok := parseStoredTime(firstTS.String); ok {
			c.FirstWaitingAt = t
		}
		if t, ok := parseStoredTime(lastTS.String); ok {
			c.LastWaitingAt = t
		}
		if t, ok := parseStoredTime(lastOut.String); lastOut.Valid && ok {
			c.LastReplyAt = &t
		}
		result = append(result, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
