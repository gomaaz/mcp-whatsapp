package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestQuoteSnippet(t *testing.T) {
	cases := []struct {
		name, text, mediaType, filename, want string
	}{
		{"empty", "", "", "", ""},
		{"text", "see you at 5", "", "", "see you at 5"},
		{"newlines collapse", "line one\n\nline  two", "", "", "line one line two"},
		{"image without caption", "", "image", "image_abc.jpg", "[image]"},
		{"image with caption", "look", "image", "image_abc.jpg", "[image] look"},
		{"document named", "", "document", "Angebot.pdf", "[document: Angebot.pdf]"},
		{"document unnamed", "", "document", "", "[document]"},
		{"sticker", "", "sticker", "", "[sticker]"},
	}
	for _, tc := range cases {
		if got := QuoteSnippet(tc.text, tc.mediaType, tc.filename); got != tc.want {
			t.Errorf("%s: QuoteSnippet = %q, want %q", tc.name, got, tc.want)
		}
	}

	long := strings.Repeat("ä", 500)
	got := QuoteSnippet(long, "", "")
	if n := utf8.RuneCountInString(got); n != maxQuoteRunes {
		t.Errorf("long snippet has %d runes, want %d", n, maxQuoteRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long snippet should end with an ellipsis, got %q", got[len(got)-10:])
	}
}

func TestFormatMessage_ReplyAndForwarded(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	m := Message{
		ID:              "r1",
		ChatJID:         "123456789@g.us",
		Sender:          "447700000002",
		Content:         "yes, 5 works",
		Timestamp:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		QuotedMessageID: "g1",
		QuotedSender:    "447700000001", // Alice, per seed data
		QuotedText:      `can we meet at "5"?`,
		IsForwarded:     true,
	}
	got := s.FormatMessage(ctx, m, false)
	want := `From: Bob: [forwarded] (reply to g1 from Alice: "can we meet at \"5\"?") yes, 5 works` + "\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("got %q, want suffix %q", got, want)
	}
}

func TestFormatMessage_ReplyVariants(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := Message{
		ID:        "r2",
		ChatJID:   "447700000001@s.whatsapp.net",
		Sender:    "447700000001",
		Content:   "ok",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	fromMe := base
	fromMe.QuotedMessageID = "a4"
	fromMe.QuotedFromMe = true
	fromMe.QuotedText = "[image] see attached"
	if got := s.FormatMessage(ctx, fromMe, false); !strings.Contains(got, `(reply to a4 from Me: "[image] see attached") ok`) {
		t.Errorf("reply to own message: got %q", got)
	}

	bare := base
	bare.QuotedMessageID = "unknown-id"
	if got := s.FormatMessage(ctx, bare, false); !strings.Contains(got, ": (reply to unknown-id) ok") {
		t.Errorf("reply without author or text: got %q", got)
	}

	if got := s.FormatMessage(ctx, base, false); strings.Contains(got, "reply to") || strings.Contains(got, "[forwarded]") {
		t.Errorf("plain message must carry no markers: got %q", got)
	}
}

// TestGetMessageContext_QuotedOriginal checks the read path: the cached
// original wins over the snippet embedded in the reply, the embedded snippet
// is kept when the original is not cached, and both reach the JSON output.
func TestGetMessageContext_QuotedOriginal(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const alice = "447700000001@s.whatsapp.net"

	replies := []Message{
		{ID: "q1", ChatJID: alice, Sender: "447700000001", Content: "reply to cached",
			Timestamp:       time.Date(2026, 1, 9, 11, 0, 0, 0, time.UTC),
			QuotedMessageID: "a1", QuotedSender: "447700000001", QuotedText: "stale copy"},
		{ID: "q2", ChatJID: alice, Sender: "447700000001", Content: "reply to my picture",
			Timestamp:       time.Date(2026, 1, 9, 11, 1, 0, 0, time.UTC),
			QuotedMessageID: "a4", QuotedText: "[image]"},
		{ID: "q3", ChatJID: alice, Sender: "447700000001", Content: "reply to uncached",
			Timestamp:       time.Date(2026, 1, 9, 11, 2, 0, 0, time.UTC),
			QuotedMessageID: "gone", QuotedSender: "447700000001", QuotedText: "embedded snippet",
			IsForwarded: true},
	}
	for _, m := range replies {
		if err := s.StoreMessage(ctx, m, nil, nil, nil, 0); err != nil {
			t.Fatalf("store %s: %v", m.ID, err)
		}
	}

	mc, err := s.GetMessageContext(ctx, "q2", 1, 1)
	if err != nil {
		t.Fatalf("GetMessageContext: %v", err)
	}
	if len(mc.Before) != 1 || len(mc.After) != 1 {
		t.Fatalf("window = %d before / %d after, want 1/1", len(mc.Before), len(mc.After))
	}
	cached, mine, uncached := mc.Before[0], mc.Message, mc.After[0]

	if cached.QuotedMessageID != "a1" || cached.QuotedText != "hello alice" || cached.QuotedSender != "447700000001" || cached.QuotedFromMe {
		t.Errorf("reply to cached original = %+v", cached)
	}
	if mine.QuotedText != "[image] see attached" || !mine.QuotedFromMe || mine.QuotedSender != "me" {
		t.Errorf("reply to own picture = %+v", mine)
	}
	if uncached.QuotedText != "embedded snippet" || uncached.QuotedFromMe || !uncached.IsForwarded {
		t.Errorf("reply to uncached original = %+v", uncached)
	}

	raw, err := json.Marshal(mc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"quoted_message_id":"a4"`, `"quoted_text":"[image] see attached"`, `"quoted_from_me":true`, `"is_forwarded":true`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON lacks %s: %s", key, raw)
		}
	}

	// list_messages goes through the same projection.
	msgs, err := s.ListMessages(ctx, ListMessagesParams{ChatJID: alice, Query: "reply to cached"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].QuotedText != "hello alice" {
		t.Fatalf("ListMessages = %+v", msgs)
	}
	line := s.FormatMessagesList(ctx, msgs, true)
	if !strings.Contains(line, `(reply to a1 from Alice: "hello alice") reply to cached`) {
		t.Errorf("formatted line = %q", line)
	}
}

// TestMigrateSchema_AddsQuoteColumns: a database from before the reply
// columns gets them on Open, keeps its rows, and Open stays idempotent.
func TestMigrateSchema_AddsQuoteColumns(t *testing.T) {
	dir := t.TempDir()
	legacy, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open legacy DB: %v", err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, content TEXT,
			timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, direct_path TEXT,
			media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
			file_length INTEGER, poll_options_json TEXT,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
		INSERT INTO chats (jid, name) VALUES ('legacy@s.whatsapp.net', 'Legacy');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('legacy-1', 'legacy@s.whatsapp.net', 'legacy', 'old text', '2026-01-01 09:00:00', 0);
	`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	_ = legacy.Close()

	for i := 0; i < 2; i++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		msgs, err := s.ListMessages(context.Background(), ListMessagesParams{ChatJID: "legacy@s.whatsapp.net", Query: "old text"})
		if err != nil {
			s.Close()
			t.Fatalf("ListMessages after migration #%d: %v", i+1, err)
		}
		if len(msgs) != 1 || msgs[0].Content != "old text" || msgs[0].QuotedMessageID != "" || msgs[0].IsForwarded {
			t.Errorf("legacy row after migration #%d = %+v", i+1, msgs)
		}
		if err := s.StoreMessage(context.Background(), Message{
			ID: "new-1", ChatJID: "legacy@s.whatsapp.net", Sender: "legacy", Content: "reply",
			Timestamp: time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC), QuotedMessageID: "legacy-1",
		}, nil, nil, nil, 0); err != nil {
			t.Errorf("StoreMessage with quote after migration #%d: %v", i+1, err)
		}
		// The reply finds the legacy original through the quoted join.
		mc, err := s.GetMessageContext(context.Background(), "new-1", 1, 1)
		if err != nil || mc.Message.QuotedText != "old text" {
			t.Errorf("reply to legacy row after migration #%d: %+v, %v", i+1, mc, err)
		}
		s.Close()
	}
}
