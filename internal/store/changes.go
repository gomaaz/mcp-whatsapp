package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MessageChange identifies a cached message that an edit or a revoke (delete
// for everyone) refers to, and who sent that edit or revoke.
type MessageChange struct {
	// ChatJID and ID select the target row. ChatJID must be the chat the
	// protocol message arrived in (LID-resolved), never the RemoteJID from
	// the message key, so a change cannot reach into another chat.
	ChatJID string
	ID      string
	// ActorSenders are the bare user parts that identify the sender of the
	// edit or revoke (phone number and/or LID); any of them may match.
	ActorSenders []string
	// ActorFromMe is true when the change came from one of our own devices.
	ActorFromMe bool
	// At is when the change was made; it is stored as edited_at/revoked_at.
	At time.Time
}

// ChangeResult reports what ApplyEdit or ApplyRevoke did.
type ChangeResult int

const (
	// ChangeApplied means the row was updated.
	ChangeApplied ChangeResult = iota
	// ChangeTargetMissing means the message is not cached (yet).
	ChangeTargetMissing
	// ChangeSenderMismatch means the change did not come from the author of
	// the message (for example a group admin deleting someone else's
	// message) and was ignored.
	ChangeSenderMismatch
	// ChangeTargetRevoked means an edit arrived for a deleted message and
	// was ignored.
	ChangeTargetRevoked
	// ChangeStale means the message already carries a newer edit, so an
	// older one (replayed by a history sync, which lists newest first) was
	// ignored.
	ChangeStale
)

func (r ChangeResult) String() string {
	switch r {
	case ChangeApplied:
		return "applied"
	case ChangeTargetMissing:
		return "target not cached"
	case ChangeSenderMismatch:
		return "not sent by the message's author"
	case ChangeTargetRevoked:
		return "target already deleted"
	case ChangeStale:
		return "target has a newer edit"
	}
	return fmt.Sprintf("ChangeResult(%d)", int(r))
}

// ApplyEdit replaces the text of a cached message with its edited version
// and records edited_at. It only applies when the edit comes from the
// message's author (see changeAllowed), never to a revoked message, and
// never over a newer edit: an edit older than the stored edited_at returns
// ChangeStale, so edits replayed out of order cannot roll the text back.
func (s *Store) ApplyEdit(ctx context.Context, ch MessageChange, text string) (ChangeResult, error) {
	target, revoked, err := s.changeTarget(ctx, ch)
	if err != nil || target != ChangeApplied {
		return target, err
	}
	if revoked {
		return ChangeTargetRevoked, nil
	}
	at := dbTime(ch.At)
	res, err := s.db.ExecContext(ctx,
		"UPDATE messages SET content = ?, edited_at = ? WHERE id = ? AND chat_jid = ? AND (edited_at IS NULL OR edited_at <= ?)",
		text, at, ch.ID, ch.ChatJID, at,
	)
	if err != nil {
		return 0, fmt.Errorf("apply edit: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("apply edit: %w", err)
	} else if n == 0 {
		return ChangeStale, nil
	}
	return ChangeApplied, nil
}

// ApplyRevoke marks a cached message as deleted by its sender and records
// revoked_at. Unless keepContent is set, it also blanks the text and the
// media download fields (url, direct path, key, hashes) and any cached
// transcript, so neither the text nor the file of a message its author
// deleted can be read back. The copy of its text that replies to it carried
// (quoted_text) is cleared as well; the read path shows such quotes as
// "[deleted by sender]" from then on, also for replies stored again later.
// The media type and file name stay, so the line still shows what kind of
// message it was. Revoking an already revoked message keeps the first
// revoked_at.
func (s *Store) ApplyRevoke(ctx context.Context, ch MessageChange, keepContent bool) (ChangeResult, error) {
	target, _, err := s.changeTarget(ctx, ch)
	if err != nil || target != ChangeApplied {
		return target, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("apply revoke: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := "UPDATE messages SET revoked_at = COALESCE(revoked_at, ?)"
	if !keepContent {
		q += ", content = '', url = '', direct_path = '', media_key = NULL, file_sha256 = NULL, file_enc_sha256 = NULL, file_length = 0, transcript = NULL"
	}
	q += " WHERE id = ? AND chat_jid = ?"
	if _, err := tx.ExecContext(ctx, q, dbTime(ch.At), ch.ID, ch.ChatJID); err != nil {
		return 0, fmt.Errorf("apply revoke: %w", err)
	}
	if !keepContent {
		if _, err := tx.ExecContext(ctx,
			"UPDATE messages SET quoted_text = NULL WHERE chat_jid = ? AND quoted_message_id = ?",
			ch.ChatJID, ch.ID,
		); err != nil {
			return 0, fmt.Errorf("apply revoke: clear quotes: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("apply revoke: %w", err)
	}
	return ChangeApplied, nil
}

// changeTarget looks up the target row of ch and checks that the change may
// be applied to it. It returns ChangeApplied when it may, and whether the
// row is already revoked.
func (s *Store) changeTarget(ctx context.Context, ch MessageChange) (ChangeResult, bool, error) {
	var (
		sender   sql.NullString
		isFromMe bool
		revoked  sql.NullTime
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT sender, is_from_me, revoked_at FROM messages WHERE id = ? AND chat_jid = ?", ch.ID, ch.ChatJID,
	).Scan(&sender, &isFromMe, &revoked)
	if err == sql.ErrNoRows {
		return ChangeTargetMissing, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read change target: %w", err)
	}
	if !s.changeAllowed(sender.String, isFromMe, ch) {
		return ChangeSenderMismatch, revoked.Valid, nil
	}
	return ChangeApplied, revoked.Valid, nil
}

// changeAllowed reports whether ch may change a message stored with the
// given sender and direction. Our own messages may only be changed from our
// own devices. Anyone else's message only by its author: the stored sender
// and one of the actor senders must name the same user, directly or through
// the LID map. Group admins deleting other people's messages are not
// honoured, since telling an admin apart would need a network lookup.
func (s *Store) changeAllowed(storedSender string, storedFromMe bool, ch MessageChange) bool {
	if storedFromMe {
		return ch.ActorFromMe
	}
	if ch.ActorFromMe || storedSender == "" {
		return false
	}
	want := s.resolveSenderPhone(storedSender)
	for _, actor := range ch.ActorSenders {
		if actor == "" {
			continue
		}
		if actor == storedSender || s.resolveSenderPhone(actor) == want {
			return true
		}
	}
	return false
}

// nullableDBTime renders t with dbTime, or returns nil (SQL NULL) for a nil t.
func nullableDBTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return dbTime(*t)
}
