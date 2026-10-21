package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Reaction is one person's current emoji reaction to a cached message.
type Reaction struct {
	// Sender is the bare phone/user part of the reacting person,
	// LID-resolved when the mapping is known.
	Sender    string    `json:"sender"`
	IsFromMe  bool      `json:"is_from_me"`
	Emoji     string    `json:"emoji"`
	Timestamp time.Time `json:"timestamp"` // UTC
}

// ReactionUpdate is a reaction as it arrives from WhatsApp: who reacted with
// what to which message. An empty Emoji removes that person's reaction.
type ReactionUpdate struct {
	// ChatJID must be the (LID-resolved) chat the reaction arrived in, never
	// the RemoteJID of the reaction's message key.
	ChatJID   string
	MessageID string
	Sender    string
	IsFromMe  bool
	Emoji     string
	Timestamp time.Time
}

// reactionsTable holds the newest reaction of each person to each message.
// Reactions are kept out of the messages table so they neither show up as
// chat previews nor count as messages in list_unanswered. A removal is kept
// as a row with an empty emoji, so an older reaction replayed later by a
// history sync cannot bring it back.
const reactionsTable = `
CREATE TABLE IF NOT EXISTS reactions (
	message_id TEXT NOT NULL,
	chat_jid TEXT NOT NULL,
	sender TEXT NOT NULL,
	is_from_me BOOLEAN NOT NULL DEFAULT 0,
	emoji TEXT NOT NULL,
	timestamp TIMESTAMP NOT NULL,
	PRIMARY KEY (message_id, chat_jid, sender)
)`

// StoreReaction records r as the sender's current reaction to the message.
// The target message does not have to be cached. A reaction older than the
// one already stored for the same sender and message is ignored, so a
// history sync cannot roll a reaction back.
func (s *Store) StoreReaction(ctx context.Context, r ReactionUpdate) error {
	if r.ChatJID == "" || r.MessageID == "" || r.Sender == "" {
		return fmt.Errorf("store reaction: chat, message and sender are required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reactions (message_id, chat_jid, sender, is_from_me, emoji, timestamp)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id, chat_jid, sender) DO UPDATE SET
			is_from_me = excluded.is_from_me,
			emoji = excluded.emoji,
			timestamp = excluded.timestamp
		WHERE excluded.timestamp >= reactions.timestamp`,
		r.MessageID, r.ChatJID, r.Sender, r.IsFromMe, r.Emoji, dbTime(r.Timestamp),
	)
	if err != nil {
		return fmt.Errorf("store reaction: %w", err)
	}
	return nil
}

// attachReactions loads the current reactions of msgs (removals left out)
// into their Reactions field, oldest first.
func (s *Store) attachReactions(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	index := make(map[string][]int, len(msgs))
	var (
		pairs []string
		args  []any
	)
	for i, m := range msgs {
		key := m.ChatJID + "\x00" + m.ID
		if _, ok := index[key]; !ok {
			pairs = append(pairs, "(?, ?)")
			args = append(args, m.ChatJID, m.ID)
		}
		index[key] = append(index[key], i)
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH wanted(chat_jid, message_id) AS (VALUES `+strings.Join(pairs, ", ")+`)
		SELECT r.chat_jid, r.message_id, r.sender, r.is_from_me, r.emoji, r.timestamp
		FROM reactions AS r
		JOIN wanted ON wanted.chat_jid = r.chat_jid AND wanted.message_id = r.message_id
		WHERE r.emoji <> ''
		ORDER BY r.timestamp, r.sender`, args...)
	if err != nil {
		return fmt.Errorf("load reactions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			chatJID, messageID string
			r                  Reaction
		)
		if err := rows.Scan(&chatJID, &messageID, &r.Sender, &r.IsFromMe, &r.Emoji, &r.Timestamp); err != nil {
			return fmt.Errorf("scan reaction: %w", err)
		}
		r.Timestamp = r.Timestamp.UTC()
		r.Sender = s.resolveSenderPhone(r.Sender)
		for _, i := range index[chatJID+"\x00"+messageID] {
			msgs[i].Reactions = append(msgs[i].Reactions, r)
		}
	}
	return rows.Err()
}

// formatReactions renders the reaction marker of a message line, e.g.
// "[reactions: 👍 Alice, ❤️ Me]", or "" when nobody reacted.
func (s *Store) formatReactions(ctx context.Context, reactions []Reaction) string {
	if len(reactions) == 0 {
		return ""
	}
	parts := make([]string, 0, len(reactions))
	for _, r := range reactions {
		name := "Me"
		if !r.IsFromMe {
			name = s.GetSenderName(ctx, r.Sender)
		}
		parts = append(parts, r.Emoji+" "+name)
	}
	return "[reactions: " + strings.Join(parts, ", ") + "]"
}

// createReactionsTable adds the reactions table to databases created before
// it existed. CREATE TABLE IF NOT EXISTS keeps it idempotent.
func createReactionsTable(db *sql.DB) error {
	if _, err := db.Exec(reactionsTable); err != nil {
		return fmt.Errorf("create reactions table: %w", err)
	}
	return nil
}
