package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const voiceChat = "447700000001@s.whatsapp.net"

func storeVoiceNote(t *testing.T, s *Store, id string, seconds uint32, voiceNote bool) {
	t.Helper()
	if err := s.StoreMessage(context.Background(), Message{
		ID: id, ChatJID: voiceChat, Sender: "447700000001",
		Timestamp: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		MediaType: "audio", Filename: "audio_" + id + ".ogg",
		AudioSeconds: seconds, IsVoiceNote: voiceNote,
	}, []byte{1}, []byte{2}, []byte{3}, 10); err != nil {
		t.Fatalf("store voice note: %v", err)
	}
}

func TestStoreMessage_AudioFieldsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	storeVoiceNote(t, s, "v1", 45, true)

	msgs, err := s.ListMessages(ctx, ListMessagesParams{ChatJID: voiceChat, MediaType: "audio"})
	if err != nil {
		t.Fatal(err)
	}
	var got *Message
	for i := range msgs {
		if msgs[i].ID == "v1" {
			got = &msgs[i]
		}
	}
	if got == nil {
		t.Fatalf("v1 not listed: %+v", msgs)
	}
	if got.AudioSeconds != 45 || !got.IsVoiceNote || got.Transcript != "" {
		t.Fatalf("audio fields = %d/%v/%q, want 45/true/empty", got.AudioSeconds, got.IsVoiceNote, got.Transcript)
	}

	mc, err := s.GetMessageContext(ctx, "v1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if mc.Message.AudioSeconds != 45 || !mc.Message.IsVoiceNote {
		t.Fatalf("context target audio fields = %d/%v", mc.Message.AudioSeconds, mc.Message.IsVoiceNote)
	}
}

func TestSetTranscript_GetAudioInfoAndRedelivery(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	storeVoiceNote(t, s, "v2", 12, true)

	ok, err := s.SetTranscript(ctx, "v2", voiceChat, "Bitte um Rückruf", false)
	if err != nil || !ok {
		t.Fatalf("SetTranscript = %v, %v", ok, err)
	}
	info, err := s.GetAudioInfo(ctx, "v2", voiceChat)
	if err != nil {
		t.Fatal(err)
	}
	want := AudioInfo{MediaType: "audio", AudioSeconds: 12, IsVoiceNote: true, Transcript: "Bitte um Rückruf"}
	if info != want {
		t.Fatalf("GetAudioInfo = %+v, want %+v", info, want)
	}

	// History sync delivering the same message again keeps the transcript.
	storeVoiceNote(t, s, "v2", 12, true)
	info, err = s.GetAudioInfo(ctx, "v2", voiceChat)
	if err != nil {
		t.Fatal(err)
	}
	if info.Transcript != "Bitte um Rückruf" {
		t.Fatalf("transcript after redelivery = %q", info.Transcript)
	}

	if _, err := s.GetAudioInfo(ctx, "missing", voiceChat); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("missing message err = %v, want ErrMessageNotFound", err)
	}
}

func TestSetTranscript_SkipsNonAudioAndRevoked(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.StoreMessage(ctx, Message{
		ID: "t1", ChatJID: voiceChat, Sender: "447700000001", Content: "text",
		Timestamp: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	}, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetTranscript(ctx, "t1", voiceChat, "x", false); err != nil || ok {
		t.Fatalf("SetTranscript on text = %v, %v; want false", ok, err)
	}

	storeVoiceNote(t, s, "v3", 5, true)
	if _, err := s.ApplyRevoke(ctx, MessageChange{
		ChatJID: voiceChat, ID: "v3", ActorSenders: []string{"447700000001"},
		At: time.Date(2026, 3, 1, 10, 1, 0, 0, time.UTC),
	}, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetTranscript(ctx, "v3", voiceChat, "x", false); err != nil || ok {
		t.Fatalf("SetTranscript on revoked = %v, %v; want false", ok, err)
	}
	// With -keep-revoked the text of a deleted message is kept, so its
	// transcript may be cached too.
	if ok, err := s.SetTranscript(ctx, "v3", voiceChat, "x", true); err != nil || !ok {
		t.Fatalf("SetTranscript on revoked with keepRevoked = %v, %v; want true", ok, err)
	}
}

func TestApplyRevoke_BlanksTranscriptUnlessKept(t *testing.T) {
	for _, keep := range []bool{false, true} {
		s := openTestStore(t)
		ctx := context.Background()
		storeVoiceNote(t, s, "v4", 5, true)
		if _, err := s.SetTranscript(ctx, "v4", voiceChat, "geheim", false); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyRevoke(ctx, MessageChange{
			ChatJID: voiceChat, ID: "v4", ActorSenders: []string{"447700000001"},
			At: time.Date(2026, 3, 1, 10, 1, 0, 0, time.UTC),
		}, keep); err != nil {
			t.Fatal(err)
		}
		info, err := s.GetAudioInfo(ctx, "v4", voiceChat)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if keep {
			want = "geheim"
		}
		if info.Transcript != want || !info.Revoked {
			t.Fatalf("keep=%v: transcript=%q revoked=%v, want %q/true", keep, info.Transcript, info.Revoked, want)
		}
	}
}

func TestListMessages_QueryMatchesTranscript(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	storeVoiceNote(t, s, "v5", 30, true)
	if _, err := s.SetTranscript(ctx, "v5", voiceChat, "Termin am Donnerstag", false); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(ctx, ListMessagesParams{Query: "DONNERSTAG"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != "v5" || msgs[0].Transcript != "Termin am Donnerstag" {
		t.Fatalf("query on transcript = %+v", msgs)
	}
	// Plain content search still works and does not trip over NULL transcripts.
	msgs, err = s.ListMessages(ctx, ListMessagesParams{Query: "Donnerstag-gibt-es-nicht"})
	if err != nil || len(msgs) != 0 {
		t.Fatalf("non-matching query = %+v, %v", msgs, err)
	}
}

func TestFormatMessage_AudioLabelsAndTranscript(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := Message{
		ID: "a1", ChatJID: voiceChat, Sender: "me", IsFromMe: true,
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), MediaType: "audio",
	}
	prefix := "[2026-01-02T03:04:05Z] [message_id=a1 chat_jid=" + voiceChat + "] From: Me: "
	cases := []struct {
		name string
		edit func(*Message)
		want string
	}{
		{"voice note with length", func(m *Message) { m.IsVoiceNote, m.AudioSeconds = true, 45 }, "[voice note, 45 s] "},
		{"voice note without length", func(m *Message) { m.IsVoiceNote = true }, "[voice note] "},
		{"audio file", func(m *Message) { m.AudioSeconds = 3 }, "[audio, 3 s] "},
		{"legacy audio row", func(*Message) {}, "[audio] "},
		{"transcript on one line", func(m *Message) {
			m.IsVoiceNote, m.AudioSeconds, m.Transcript = true, 45, " Hallo,\n ich bin's. "
		}, `[voice note, 45 s] transcript: "Hallo, ich bin's."`},
		{"caption and transcript", func(m *Message) { m.Content, m.Transcript = "hör mal", "Text" }, `[audio] hör mal transcript: "Text"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			tc.edit(&m)
			got := s.FormatMessage(ctx, m, false)
			if want := prefix + tc.want + "\n"; got != want {
				t.Fatalf("FormatMessage:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// A database from before the audio columns gets them added on Open, keeps
// its rows, and opens again without error.
func TestMigrateSchema_AddsAudioColumns(t *testing.T) {
	dir := t.TempDir()
	legacy, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, content TEXT,
			timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, direct_path TEXT,
			media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB, file_length INTEGER,
			poll_options_json TEXT, quoted_message_id TEXT, quoted_sender TEXT, quoted_text TEXT,
			is_forwarded BOOLEAN DEFAULT 0, edited_at TIMESTAMP, revoked_at TIMESTAMP,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
		INSERT INTO chats (jid, name) VALUES ('old@s.whatsapp.net', 'Old');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type)
		VALUES ('old-voice', 'old@s.whatsapp.net', 'old', '', '2026-01-01T09:00:00Z', 0, 'audio');
	`); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	for i := 0; i < 2; i++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		info, err := s.GetAudioInfo(context.Background(), "old-voice", "old@s.whatsapp.net")
		if err != nil {
			s.Close()
			t.Fatalf("GetAudioInfo after migration #%d: %v", i+1, err)
		}
		if info.MediaType != "audio" || info.AudioSeconds != 0 || info.IsVoiceNote || info.Transcript != "" {
			s.Close()
			t.Fatalf("legacy row after migration = %+v", info)
		}
		msgs, err := s.ListMessages(context.Background(), ListMessagesParams{ChatJID: "old@s.whatsapp.net"})
		s.Close()
		if err != nil || len(msgs) != 1 {
			t.Fatalf("ListMessages after migration #%d = %+v, %v", i+1, msgs, err)
		}
		if line := strings.TrimSpace(msgs[0].Content); line != "" {
			t.Fatalf("unexpected content %q", line)
		}
	}
}
