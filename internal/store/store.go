// Package store owns the SQLite-backed message/chat cache and the mapping layer
// to whatsmeow's own SQLite database. The schema is kept byte-compatible with
// the historical layout so existing store/messages.db files keep working.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ChmodBestEffort tightens a path's permissions but tolerates filesystems that
// do not support chmod. Network mounts such as Azure Files / CIFS SMB return
// EPERM/ENOTSUP/EINVAL from chmod(2) — on those mounts access is governed by
// the mount options (uid/gid/file_mode), not POSIX mode bits, so a rejected
// chmod is not a security regression. A local filesystem never returns these
// errors for a file the process owns, so genuine permission tightening on
// local disk is preserved. Missing files are likewise tolerated.
func ChmodBestEffort(path string, mode os.FileMode) error {
	err := os.Chmod(path, mode)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}

// Message mirrors the on-wire shape used by downstream consumers.
type Message struct {
	ID          string    `json:"id"`
	ChatJID     string    `json:"chat_jid"`
	Sender      string    `json:"sender"`                 // bare phone/user part
	SenderPhone string    `json:"sender_phone,omitempty"` // full phone if resolvable
	Content     string    `json:"content"`
	Timestamp   time.Time `json:"timestamp"` // UTC when read from the store
	IsFromMe    bool      `json:"is_from_me"`
	IsGroup     bool      `json:"is_group"`
	MediaType   string    `json:"media_type,omitempty"`
	Filename    string    `json:"filename,omitempty"`
	URL         string    `json:"url,omitempty"`
	// DirectPath is the CDN path (starting with `/`, including the signed
	// `?ccb=&oh=&oe=&_nc_sid=` auth query) captured verbatim from the
	// WhatsApp media protobuf at receive/send time. When present, the
	// download path uses it directly; when absent (legacy row written by
	// an older daemon build), Download falls back to parsing URL.
	DirectPath  string `json:"direct_path,omitempty"`
	ChatName    string `json:"chat_name,omitempty"`
	PhoneNumber string `json:"phone_number,omitempty"`
	// QuotedMessageID is the ID of the message this one replies to (the
	// ContextInfo stanza ID), empty when it is not a reply.
	QuotedMessageID string `json:"quoted_message_id,omitempty"`
	// QuotedSender is the bare phone/user part of the quoted message's
	// author, LID-resolved when the mapping was known.
	QuotedSender string `json:"quoted_sender,omitempty"`
	// QuotedText is a short snippet of the quoted message (see QuoteSnippet).
	// Reads prefer the cached original over the snippet embedded in the reply.
	QuotedText string `json:"quoted_text,omitempty"`
	// QuotedFromMe is set on read when the cached original is our own
	// message. It is not stored.
	QuotedFromMe bool `json:"quoted_from_me,omitempty"`
	// IsForwarded marks a message WhatsApp flagged as forwarded.
	IsForwarded bool `json:"is_forwarded,omitempty"`
	// EditedAt is when the sender last edited the message; nil if never.
	EditedAt *time.Time `json:"edited_at,omitempty"`
	// RevokedAt is when the sender deleted the message for everyone; nil if
	// not. Unless the daemon runs with -keep-revoked, the stored text and
	// media keys of a revoked message are blanked (see ApplyRevoke).
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// AudioSeconds is the duration WhatsApp reported for an audio message,
	// 0 when unknown or not audio.
	AudioSeconds uint32 `json:"audio_seconds,omitempty"`
	// IsVoiceNote marks an audio message recorded as a voice note (PTT)
	// rather than sent as an audio file.
	IsVoiceNote bool `json:"is_voice_note,omitempty"`
	// Transcript is the cached local transcription of an audio message
	// (see transcribe_media); empty until one was made.
	Transcript string `json:"transcript,omitempty"`
	// rowID is the SQLite rowid of the cached row, set by the list/context
	// queries. It breaks ties between messages that share a timestamp
	// (a screenshot and its follow-up text sent in the same second).
	rowID int64
}

// Chat is a cached WhatsApp chat.
type Chat struct {
	JID             string    `json:"jid"`
	Name            string    `json:"name"`
	LastMessageTime time.Time `json:"last_message_time"` // UTC
	LastMessage     string    `json:"last_message,omitempty"`
	LastSender      string    `json:"last_sender,omitempty"`
	LastIsFromMe    bool      `json:"last_is_from_me"`
	PhoneNumber     string    `json:"phone_number,omitempty"`
	IsGroup         bool      `json:"is_group"`
}

// Contact is a directory entry derived from chat history.
type Contact struct {
	PhoneNumber string `json:"phone_number"`
	Name        string `json:"name,omitempty"`
	JID         string `json:"jid"`
}

// MessageContext wraps a target message with surrounding context.
type MessageContext struct {
	Message Message   `json:"message"`
	Before  []Message `json:"before,omitempty"`
	After   []Message `json:"after,omitempty"`
}

// Store handles the message cache and read-only access to whatsmeow's DB.
type Store struct {
	db          *sql.DB
	whatsmeowDB *sql.DB
	dir         string
}

// Open creates (if necessary) the store directory and opens both databases.
// Dir is the directory holding messages.db and whatsapp.db. The directory is
// created/tightened to 0700 and each .db file ends up at 0600 to keep session
// material and cached message contents off other local users' radar.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	if err := ChmodBestEffort(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod store dir: %w", err)
	}
	msgPath := filepath.Join(dir, "messages.db")
	waPath := filepath.Join(dir, "whatsapp.db")

	db, err := sql.Open("sqlite3", "file:"+msgPath+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open messages db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	// Tighten messages.db after schema has been applied (the file exists
	// by then). IsNotExist-tolerate in case a future sqlite driver version
	// defers file creation.
	if err := ChmodBestEffort(msgPath, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("chmod messages.db: %w", err)
	}

	// whatsapp.db may not yet exist on first run; open lazily in read-only mode.
	waDB, err := sql.Open("sqlite3", "file:"+waPath+"?mode=ro")
	if err != nil {
		// Non-fatal: LID resolution falls through to identity.
		waDB = nil
	}
	// If whatsapp.db already exists (from a previous login), tighten it.
	if err := ChmodBestEffort(waPath, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("chmod whatsapp.db: %w", err)
	}

	return &Store{db: db, whatsmeowDB: waDB, dir: dir}, nil
}

// DB returns the underlying message database handle (tests only).
func (s *Store) DB() *sql.DB { return s.db }

// Dir returns the directory backing the store.
func (s *Store) Dir() string { return s.dir }

// Close releases both database connections.
func (s *Store) Close() error {
	if s.whatsmeowDB != nil {
		_ = s.whatsmeowDB.Close()
	}
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// StoreChat upserts a chat row. An existing row keeps its name when the new
// one is empty, and its last_message_time only ever moves forward, so a
// history sync of old messages cannot rewind a chat's recency. The time is
// written as UTC text (dbTime); the comparison goes through julianday() so a
// row in any older text format still compares correctly.
func (s *Store) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			name = COALESCE(NULLIF(excluded.name, ''), chats.name),
			last_message_time = CASE
				WHEN julianday(chats.last_message_time) IS NULL
					OR julianday(excluded.last_message_time) > julianday(chats.last_message_time)
				THEN excluded.last_message_time
				ELSE chats.last_message_time
			END`,
		jid, name, dbTime(lastMessageTime),
	)
	return err
}

// StoreMessage inserts or replaces a message row. Messages with no content and
// no media type are dropped to avoid polluting the cache with signal-only
// events (typing, receipts, key exchange, etc.).
//
// A message delivered again (history sync re-sending a cached message) must
// not undo what the sender did to it later: a revoked row is left as it is,
// and an edited row keeps its edited text unless m carries a newer edit
// (m.EditedAt set and not before the stored one). A cached transcript is
// carried over, since the same message ID carries the same audio.
//
// A row can also hold an edit whose original was not cached (see
// storeHistoryEdit in the client). Its text only survives the arrival of
// the original when both come from the same author, by the same rule as
// changeAllowed; otherwise the original replaces the row and its edited_at
// is cleared. An edit (m.EditedAt set) never replaces a row of another
// author.
//
// When m quotes a cached message its sender deleted, the copy of that
// message's text embedded in m is not stored (see ApplyRevoke).
func (s *Store) StoreMessage(ctx context.Context, m Message, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	if m.Content == "" && m.MediaType == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		oldContent    sql.NullString
		oldSender     sql.NullString
		oldFromMe     bool
		oldEdited     sql.NullTime
		oldRevoked    sql.NullTime
		oldTranscript sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		"SELECT content, sender, is_from_me, edited_at, revoked_at, transcript FROM messages WHERE id = ? AND chat_jid = ?",
		m.ID, m.ChatJID,
	).Scan(&oldContent, &oldSender, &oldFromMe, &oldEdited, &oldRevoked, &oldTranscript)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return err
	case oldRevoked.Valid:
		return nil
	case oldEdited.Valid || m.EditedAt != nil:
		sameAuthor := s.changeAllowed(oldSender.String, oldFromMe, MessageChange{
			ActorSenders: []string{m.Sender}, ActorFromMe: m.IsFromMe,
		})
		switch {
		case m.EditedAt != nil && !sameAuthor:
			return nil
		case sameAuthor && oldEdited.Valid && (m.EditedAt == nil || m.EditedAt.Before(oldEdited.Time)):
			m.Content = oldContent.String
			editedAt := oldEdited.Time
			m.EditedAt = &editedAt
		}
	}
	if m.Transcript == "" {
		m.Transcript = oldTranscript.String
	}
	if m.QuotedMessageID != "" && m.QuotedText != "" {
		var quotedRevoked sql.NullTime
		err := tx.QueryRowContext(ctx,
			"SELECT revoked_at FROM messages WHERE id = ? AND chat_jid = ?", m.QuotedMessageID, m.ChatJID,
		).Scan(&quotedRevoked)
		switch {
		case err == sql.ErrNoRows:
		case err != nil:
			return err
		case quotedRevoked.Valid:
			m.QuotedText = ""
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length,
		 quoted_message_id, quoted_sender, quoted_text, is_forwarded, edited_at, audio_seconds, is_voice_note, transcript)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ChatJID, m.Sender, m.Content, dbTime(m.Timestamp), m.IsFromMe,
		m.MediaType, m.Filename, m.URL, m.DirectPath, mediaKey, fileSHA256, fileEncSHA256, fileLength,
		m.QuotedMessageID, m.QuotedSender, m.QuotedText, m.IsForwarded, nullableDBTime(m.EditedAt),
		m.AudioSeconds, m.IsVoiceNote, nullableString(m.Transcript),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// StoreMediaInfo updates the media columns of an existing message row.
func (s *Store) StoreMediaInfo(id, chatJID, url, directPath string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := s.db.Exec(
		"UPDATE messages SET url = ?, direct_path = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// GetMediaInfo fetches the persisted media fields for a message. direct_path
// is the CDN path captured from the WhatsApp media protobuf at receive time,
// including the signed `?ccb=&oh=&oe=&_nc_sid=` auth query. It may be empty
// for legacy rows written before this column existed — callers that need
// to fetch bytes should fall back to parsing url in that case.
func (s *Store) GetMediaInfo(id, chatJID string) (mediaType, filename, url, directPath string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64, err error) {
	var dp sql.NullString
	err = s.db.QueryRow(
		"SELECT media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &dp, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)
	directPath = dp.String
	return
}

// GetNewestMessage returns the latest message row for a chat, used by the
// history-sync code to anchor the request.
func (s *Store) GetNewestMessage(chatJID string) (id string, ts time.Time, isFromMe bool, err error) {
	err = s.db.QueryRow(`
		SELECT id, timestamp, is_from_me
		FROM messages
		WHERE chat_jid = ?
		ORDER BY timestamp DESC
		LIMIT 1`, chatJID).Scan(&id, &ts, &isFromMe)
	return
}

// GetOldestMessage returns the earliest message row for a chat. It is the
// counterpart to GetNewestMessage and exists so the history-sync code can
// anchor a backward walk on a real cached message: WhatsApp resolves an
// on-demand history-sync cursor by message key, so extending history backwards
// requires the key of the oldest row we already hold, not a synthesised one.
func (s *Store) GetOldestMessage(chatJID string) (id string, ts time.Time, isFromMe bool, err error) {
	err = s.db.QueryRow(`
		SELECT id, timestamp, is_from_me
		FROM messages
		WHERE chat_jid = ?
		ORDER BY timestamp ASC
		LIMIT 1`, chatJID).Scan(&id, &ts, &isFromMe)
	return
}

// GetMessageAtOrBefore returns the newest message row at or before ts for a
// chat. It lets a caller anchor a history-sync request on a real cached
// message near a requested point in time rather than fabricating a key that
// the server cannot resolve. Returns sql.ErrNoRows when the chat has nothing
// at or before ts.
func (s *Store) GetMessageAtOrBefore(chatJID string, before time.Time) (id string, ts time.Time, isFromMe bool, err error) {
	err = s.db.QueryRow(`
		SELECT id, timestamp, is_from_me
		FROM messages
		WHERE chat_jid = ? AND timestamp <= ?
		ORDER BY timestamp DESC
		LIMIT 1`, chatJID, dbTime(before)).Scan(&id, &ts, &isFromMe)
	return
}

// HasInboundFrom reports whether the cache holds at least one message we
// received (is_from_me = 0) in the given chat. It is the "have we ever heard
// from this JID" signal the rate limiter uses to classify a send target as a
// known contact (leash) versus cold outreach (stricter leash). A group JID is
// always a known context and callers treat it as such without querying here.
func (s *Store) HasInboundFrom(chatJID string) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM messages WHERE chat_jid = ? AND is_from_me = 0 LIMIT 1)",
		chatJID,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}

// FindChatName returns the name stored for a chat JID, empty if none.
func (s *Store) FindChatName(jid string) string {
	var name string
	_ = s.db.QueryRow("SELECT name FROM chats WHERE jid = ?", jid).Scan(&name)
	return name
}

// RecentIncomingMessages returns up to `limit` recent incoming message IDs +
// their sender JIDs for the given chat. Used to drive the "mark whole chat as
// read" helper — we don't track per-message read state locally, so we treat
// recent incoming messages as the candidates to ack.
func (s *Store) RecentIncomingMessages(ctx context.Context, chatJID string, limit int) (ids []string, senders []string, err error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, sender
		FROM messages
		WHERE chat_jid = ? AND is_from_me = 0
		ORDER BY timestamp DESC
		LIMIT ?`, chatJID, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, sender string
		if err := rows.Scan(&id, &sender); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		senders = append(senders, sender)
	}
	return ids, senders, rows.Err()
}

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	jid TEXT PRIMARY KEY,
	name TEXT,
	last_message_time TIMESTAMP
);

CREATE TABLE IF NOT EXISTS messages (
	id TEXT,
	chat_jid TEXT,
	sender TEXT,
	content TEXT,
	timestamp TIMESTAMP,
	is_from_me BOOLEAN,
	media_type TEXT,
	filename TEXT,
	url TEXT,
	direct_path TEXT,
	media_key BLOB,
	file_sha256 BLOB,
	file_enc_sha256 BLOB,
	file_length INTEGER,
	poll_options_json TEXT,
	quoted_message_id TEXT,
	quoted_sender TEXT,
	quoted_text TEXT,
	is_forwarded BOOLEAN DEFAULT 0,
	edited_at TIMESTAMP,
	revoked_at TIMESTAMP,
	audio_seconds INTEGER DEFAULT 0,
	is_voice_note BOOLEAN DEFAULT 0,
	transcript TEXT,
	PRIMARY KEY (id, chat_jid),
	FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);

CREATE TABLE IF NOT EXISTS poll_votes (
	poll_message_id TEXT NOT NULL,
	poll_chat_jid TEXT NOT NULL,
	voter_jid TEXT NOT NULL,
	selected_options_json TEXT NOT NULL,
	timestamp TIMESTAMP NOT NULL,
	PRIMARY KEY (poll_message_id, poll_chat_jid, voter_jid)
);
`

// migrateSchema brings legacy databases up to the current layout. SQLite lacks
// "ADD COLUMN IF NOT EXISTS", so we inspect PRAGMA table_info before issuing
// any ALTER. Data rewrites are tracked in PRAGMA user_version. Every step is
// idempotent — running migrateSchema twice is a no-op on an up-to-date DB.
func migrateSchema(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(messages)")
	if err != nil {
		return fmt.Errorf("pragma table_info(messages): %w", err)
	}
	defer rows.Close()
	existing := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notnull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &primaryKey); err != nil {
			return fmt.Errorf("scan table_info(messages): %w", err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate table_info(messages): %w", err)
	}
	rows.Close()
	for _, col := range addedMessageColumns {
		if existing[col.name] {
			continue
		}
		if _, err := db.Exec("ALTER TABLE messages ADD COLUMN " + col.name + " " + col.def); err != nil {
			return fmt.Errorf("add column %s: %w", col.name, err)
		}
	}
	return normalizeTimestamps(db)
}

// addedMessageColumns lists the messages columns added after the original
// layout, in the order they were introduced. migrateSchema adds any that a
// legacy database lacks.
var addedMessageColumns = []struct{ name, def string }{
	{"poll_options_json", "TEXT"},
	{"direct_path", "TEXT"},
	{"quoted_message_id", "TEXT"},
	{"quoted_sender", "TEXT"},
	{"quoted_text", "TEXT"},
	{"is_forwarded", "BOOLEAN DEFAULT 0"},
	{"edited_at", "TIMESTAMP"},
	{"revoked_at", "TIMESTAMP"},
	{"audio_seconds", "INTEGER DEFAULT 0"},
	{"is_voice_note", "BOOLEAN DEFAULT 0"},
	{"transcript", "TEXT"},
}
