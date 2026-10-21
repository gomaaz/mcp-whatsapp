package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrMessageNotFound is returned by GetAudioInfo when no cached message has
// the given ID in the given chat.
var ErrMessageNotFound = errors.New("message not found in cache")

// AudioInfo is what transcribe_media needs to know about a cached message
// before it downloads anything.
type AudioInfo struct {
	MediaType    string
	AudioSeconds uint32
	IsVoiceNote  bool
	// Transcript is the cached transcript, empty if none was made yet.
	Transcript string
	// Revoked is true when the sender deleted the message for everyone.
	Revoked bool
}

// GetAudioInfo returns the media type, audio duration, voice-note flag and
// cached transcript of a message. It returns ErrMessageNotFound when the
// message is not cached.
func (s *Store) GetAudioInfo(ctx context.Context, id, chatJID string) (AudioInfo, error) {
	var (
		mediaType  sql.NullString
		seconds    sql.NullInt64
		voiceNote  sql.NullBool
		transcript sql.NullString
		revokedAt  sql.NullTime
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT media_type, audio_seconds, is_voice_note, transcript, revoked_at FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &seconds, &voiceNote, &transcript, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AudioInfo{}, ErrMessageNotFound
	}
	if err != nil {
		return AudioInfo{}, fmt.Errorf("read audio info: %w", err)
	}
	info := AudioInfo{
		MediaType:   mediaType.String,
		IsVoiceNote: voiceNote.Bool,
		Transcript:  transcript.String,
		Revoked:     revokedAt.Valid,
	}
	if seconds.Valid && seconds.Int64 > 0 {
		info.AudioSeconds = uint32(seconds.Int64)
	}
	return info, nil
}

// SetTranscript caches the transcript of an audio message. Unless
// keepRevoked is set (the daemon runs with -keep-revoked), a message the
// sender deleted for everyone is left alone, so a transcription that was
// running while the revoke arrived cannot write the text back. It reports
// whether a row was updated.
func (s *Store) SetTranscript(ctx context.Context, id, chatJID, transcript string, keepRevoked bool) (bool, error) {
	q := "UPDATE messages SET transcript = ? WHERE id = ? AND chat_jid = ? AND media_type = 'audio'"
	if !keepRevoked {
		q += " AND revoked_at IS NULL"
	}
	res, err := s.db.ExecContext(ctx, q, nullableString(transcript), id, chatJID)
	if err != nil {
		return false, fmt.Errorf("store transcript: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store transcript: %w", err)
	}
	return n > 0, nil
}

// nullableString maps "" to SQL NULL so an absent transcript is NULL rather
// than an empty string.
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
