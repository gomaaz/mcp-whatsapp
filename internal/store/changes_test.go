package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	changeDM    = "447700000001@s.whatsapp.net"
	changeGroup = "123456789@g.us"
)

var changeAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// storeChangeTarget stores one message for the edit/revoke tests.
func storeChangeTarget(t *testing.T, s *Store, m Message) {
	t.Helper()
	if m.Timestamp.IsZero() {
		m.Timestamp = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	}
	if err := s.StoreMessage(context.Background(), m, []byte{1, 2}, []byte{3, 4}, []byte{5, 6}, 99); err != nil {
		t.Fatalf("store %s: %v", m.ID, err)
	}
}

// changeRow reads back the columns an edit or revoke touches.
type changeRow struct {
	content, url, directPath, mediaType string
	mediaKey                            []byte
	edited, revoked                     sql.NullTime
}

func readChangeRow(t *testing.T, s *Store, chat, id string) changeRow {
	t.Helper()
	var r changeRow
	var url, dp, mt sql.NullString
	if err := s.db.QueryRow(
		"SELECT content, url, direct_path, media_type, media_key, edited_at, revoked_at FROM messages WHERE id = ? AND chat_jid = ?", id, chat,
	).Scan(&r.content, &url, &dp, &mt, &r.mediaKey, &r.edited, &r.revoked); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	r.url, r.directPath, r.mediaType = url.String, dp.String, mt.String
	return r
}

func TestApplyEdit_AuthorUpdatesTextAndEditedAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	storeChangeTarget(t, s, Message{ID: "e1", ChatJID: changeDM, Sender: "447700000001", Content: "Angebot bis Freitag"})

	res, err := s.ApplyEdit(ctx, MessageChange{
		ChatJID: changeDM, ID: "e1", ActorSenders: []string{"447700000001"}, At: changeAt,
	}, "Angebot bis Montag")
	if err != nil || res != ChangeApplied {
		t.Fatalf("ApplyEdit = %v, %v; want applied", res, err)
	}
	r := readChangeRow(t, s, changeDM, "e1")
	if r.content != "Angebot bis Montag" {
		t.Errorf("content = %q, want edited text", r.content)
	}
	if !r.edited.Valid || !r.edited.Time.Equal(changeAt) {
		t.Errorf("edited_at = %v, want %v", r.edited, changeAt)
	}
	if r.revoked.Valid {
		t.Errorf("revoked_at set by an edit: %v", r.revoked)
	}
}

func TestApplyEdit_SenderChecks(t *testing.T) {
	cases := []struct {
		name   string
		stored Message
		change MessageChange
		want   ChangeResult
	}{
		{
			name:   "other group member cannot edit",
			stored: Message{ID: "x", ChatJID: changeGroup, Sender: "447700000001", Content: "original"},
			change: MessageChange{ChatJID: changeGroup, ID: "x", ActorSenders: []string{"447700000002"}},
			want:   ChangeSenderMismatch,
		},
		{
			name:   "our device cannot edit someone else's message",
			stored: Message{ID: "x", ChatJID: changeDM, Sender: "447700000001", Content: "original"},
			change: MessageChange{ChatJID: changeDM, ID: "x", ActorSenders: []string{"447700000001"}, ActorFromMe: true},
			want:   ChangeSenderMismatch,
		},
		{
			name:   "contact cannot edit our message",
			stored: Message{ID: "x", ChatJID: changeDM, Sender: "491700000000", Content: "original", IsFromMe: true},
			change: MessageChange{ChatJID: changeDM, ID: "x", ActorSenders: []string{"447700000001"}},
			want:   ChangeSenderMismatch,
		},
		{
			name:   "own device edits our message",
			stored: Message{ID: "x", ChatJID: changeDM, Sender: "491700000000", Content: "original", IsFromMe: true},
			change: MessageChange{ChatJID: changeDM, ID: "x", ActorSenders: []string{"491700000000"}, ActorFromMe: true},
			want:   ChangeApplied,
		},
		{
			name:   "LID of the author matches the stored phone number",
			stored: Message{ID: "x", ChatJID: changeGroup, Sender: "447700000002", Content: "original"},
			change: MessageChange{ChatJID: changeGroup, ID: "x", ActorSenders: []string{"99887766"}},
			want:   ChangeApplied,
		},
		{
			name:   "change is scoped to its own chat",
			stored: Message{ID: "x", ChatJID: changeGroup, Sender: "447700000001", Content: "original"},
			change: MessageChange{ChatJID: changeDM, ID: "x", ActorSenders: []string{"447700000001"}},
			want:   ChangeTargetMissing,
		},
		{
			name:   "no actor at all",
			stored: Message{ID: "x", ChatJID: changeDM, Sender: "447700000001", Content: "original"},
			change: MessageChange{ChatJID: changeDM, ID: "x"},
			want:   ChangeSenderMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			storeChangeTarget(t, s, tc.stored)
			tc.change.At = changeAt
			res, err := s.ApplyEdit(context.Background(), tc.change, "changed")
			if err != nil {
				t.Fatalf("ApplyEdit: %v", err)
			}
			if res != tc.want {
				t.Fatalf("ApplyEdit = %v, want %v", res, tc.want)
			}
			r := readChangeRow(t, s, tc.stored.ChatJID, "x")
			if wantContent := map[bool]string{true: "changed", false: "original"}[res == ChangeApplied]; r.content != wantContent {
				t.Errorf("content = %q, want %q", r.content, wantContent)
			}
			if r.edited.Valid != (res == ChangeApplied) {
				t.Errorf("edited_at valid = %v, want %v", r.edited.Valid, res == ChangeApplied)
			}
		})
	}
}

func TestApplyEdit_TargetMissing(t *testing.T) {
	s := openTestStore(t)
	res, err := s.ApplyEdit(context.Background(), MessageChange{
		ChatJID: changeDM, ID: "not-yet", ActorSenders: []string{"447700000001"}, At: changeAt,
	}, "early edit")
	if err != nil || res != ChangeTargetMissing {
		t.Fatalf("ApplyEdit = %v, %v; want target missing", res, err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 'not-yet'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("an edit without its original created %d rows (err %v)", n, err)
	}
}

func TestApplyRevoke_BlanksContentAndMediaByDefault(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	storeChangeTarget(t, s, Message{
		ID: "r1", ChatJID: changeDM, Sender: "447700000001", Content: "meine IBAN",
		MediaType: "document", Filename: "Vertrag.pdf", URL: "https://mmg.whatsapp.net/x", DirectPath: "/v/x?ccb=1",
	})
	ch := MessageChange{ChatJID: changeDM, ID: "r1", ActorSenders: []string{"447700000001"}, At: changeAt}
	if res, err := s.ApplyRevoke(ctx, ch, false); err != nil || res != ChangeApplied {
		t.Fatalf("ApplyRevoke = %v, %v; want applied", res, err)
	}
	r := readChangeRow(t, s, changeDM, "r1")
	if r.content != "" || r.url != "" || r.directPath != "" || r.mediaKey != nil {
		t.Errorf("revoked row still holds content=%q url=%q direct_path=%q media_key=%v", r.content, r.url, r.directPath, r.mediaKey)
	}
	if r.mediaType != "document" {
		t.Errorf("media_type = %q, want it kept", r.mediaType)
	}
	if !r.revoked.Valid || !r.revoked.Time.Equal(changeAt) {
		t.Errorf("revoked_at = %v, want %v", r.revoked, changeAt)
	}

	// A second revoke keeps the first time; an edit after the delete is ignored.
	ch.At = changeAt.Add(time.Hour)
	if res, err := s.ApplyRevoke(ctx, ch, false); err != nil || res != ChangeApplied {
		t.Fatalf("second ApplyRevoke = %v, %v", res, err)
	}
	if res, err := s.ApplyEdit(ctx, ch, "zurück"); err != nil || res != ChangeTargetRevoked {
		t.Fatalf("ApplyEdit after revoke = %v, %v; want target revoked", res, err)
	}
	r = readChangeRow(t, s, changeDM, "r1")
	if !r.revoked.Time.Equal(changeAt) || r.content != "" || r.edited.Valid {
		t.Errorf("after second revoke and edit: revoked_at=%v content=%q edited=%v", r.revoked.Time, r.content, r.edited.Valid)
	}
}

func TestApplyRevoke_KeepContent(t *testing.T) {
	s := openTestStore(t)
	storeChangeTarget(t, s, Message{ID: "r2", ChatJID: changeDM, Sender: "447700000001", Content: "oops"})
	res, err := s.ApplyRevoke(context.Background(), MessageChange{
		ChatJID: changeDM, ID: "r2", ActorSenders: []string{"447700000001"}, At: changeAt,
	}, true)
	if err != nil || res != ChangeApplied {
		t.Fatalf("ApplyRevoke = %v, %v", res, err)
	}
	if r := readChangeRow(t, s, changeDM, "r2"); r.content != "oops" || !r.revoked.Valid {
		t.Errorf("keep-revoked: content=%q revoked=%v, want text kept and revoked_at set", r.content, r.revoked.Valid)
	}
}

func TestApplyRevoke_SenderMismatchLeavesRow(t *testing.T) {
	s := openTestStore(t)
	storeChangeTarget(t, s, Message{ID: "r3", ChatJID: changeGroup, Sender: "447700000001", Content: "stays"})
	// An admin (or anyone else) deleting someone else's message is ignored.
	res, err := s.ApplyRevoke(context.Background(), MessageChange{
		ChatJID: changeGroup, ID: "r3", ActorSenders: []string{"447700000002"}, At: changeAt,
	}, false)
	if err != nil || res != ChangeSenderMismatch {
		t.Fatalf("ApplyRevoke = %v, %v; want sender mismatch", res, err)
	}
	if r := readChangeRow(t, s, changeGroup, "r3"); r.content != "stays" || r.revoked.Valid {
		t.Errorf("row changed: content=%q revoked=%v", r.content, r.revoked.Valid)
	}
}

func TestStoreMessage_RedeliveryKeepsEditsAndRevokes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	original := Message{ID: "d1", ChatJID: changeDM, Sender: "447700000001", Content: "v1"}
	storeChangeTarget(t, s, original)
	ch := MessageChange{ChatJID: changeDM, ID: "d1", ActorSenders: []string{"447700000001"}, At: changeAt}
	if _, err := s.ApplyEdit(ctx, ch, "v2"); err != nil {
		t.Fatal(err)
	}

	// The original arrives again (history sync): the edit stays.
	storeChangeTarget(t, s, original)
	if r := readChangeRow(t, s, changeDM, "d1"); r.content != "v2" || !r.edited.Time.Equal(changeAt) {
		t.Errorf("after re-delivery: content=%q edited_at=%v, want v2 at %v", r.content, r.edited.Time, changeAt)
	}

	// An older edit loses, a newer one wins.
	older := original
	older.Content = "v1.5"
	olderAt := changeAt.Add(-time.Minute)
	older.EditedAt = &olderAt
	storeChangeTarget(t, s, older)
	if r := readChangeRow(t, s, changeDM, "d1"); r.content != "v2" {
		t.Errorf("older edit replaced newer: content=%q", r.content)
	}
	newer := original
	newer.Content = "v3"
	newerAt := changeAt.Add(time.Minute)
	newer.EditedAt = &newerAt
	storeChangeTarget(t, s, newer)
	if r := readChangeRow(t, s, changeDM, "d1"); r.content != "v3" || !r.edited.Time.Equal(newerAt) {
		t.Errorf("newer edit: content=%q edited_at=%v", r.content, r.edited.Time)
	}

	// Once revoked, a re-delivered original does not come back.
	if _, err := s.ApplyRevoke(ctx, ch, false); err != nil {
		t.Fatal(err)
	}
	storeChangeTarget(t, s, original)
	if r := readChangeRow(t, s, changeDM, "d1"); r.content != "" || !r.revoked.Valid || r.mediaKey != nil {
		t.Errorf("revoked message revived: content=%q revoked=%v media_key=%v", r.content, r.revoked.Valid, r.mediaKey)
	}
}

func TestListMessages_EditedAndRevokedFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	storeChangeTarget(t, s, Message{ID: "l1", ChatJID: changeDM, Sender: "447700000001", Content: "first"})
	storeChangeTarget(t, s, Message{ID: "l2", ChatJID: changeDM, Sender: "447700000001", Content: "second",
		Timestamp: time.Date(2026, 3, 1, 9, 1, 0, 0, time.UTC)})
	ch := MessageChange{ChatJID: changeDM, ActorSenders: []string{"447700000001"}, At: changeAt}
	ch.ID = "l1"
	if _, err := s.ApplyEdit(ctx, ch, "first, fixed"); err != nil {
		t.Fatal(err)
	}
	ch.ID = "l2"
	if _, err := s.ApplyRevoke(ctx, ch, false); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.ListMessages(ctx, ListMessagesParams{ChatJID: changeDM, After: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	byID := map[string]Message{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	if m := byID["l1"]; m.EditedAt == nil || !m.EditedAt.Equal(changeAt) || m.RevokedAt != nil || m.Content != "first, fixed" {
		t.Errorf("l1 = %+v", m)
	}
	if m := byID["l2"]; m.RevokedAt == nil || !m.RevokedAt.Equal(changeAt) || m.Content != "" {
		t.Errorf("l2 = %+v", m)
	}

	text := s.FormatMessagesList(ctx, msgs, false)
	if !strings.Contains(text, "From: Alice: [edited] first, fixed\n") {
		t.Errorf("edited line missing marker:\n%s", text)
	}
	if !strings.Contains(text, "From: Alice: [deleted by sender]\n") {
		t.Errorf("revoked line missing marker:\n%s", text)
	}

	// get_message_context returns the fields as JSON.
	mc, err := s.GetMessageContext(ctx, "l2", 1, 0)
	if err != nil {
		t.Fatalf("GetMessageContext: %v", err)
	}
	raw, err := json.Marshal(mc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"revoked_at":"2026-03-01T12:00:00Z"`) ||
		!strings.Contains(string(raw), `"edited_at":"2026-03-01T12:00:00Z"`) {
		t.Errorf("JSON lacks edited_at/revoked_at: %s", raw)
	}
	// Untouched messages carry neither field.
	plain, _ := json.Marshal(Message{ID: "p"})
	if strings.Contains(string(plain), "edited_at") || strings.Contains(string(plain), "revoked_at") {
		t.Errorf("unchanged message JSON mentions the fields: %s", plain)
	}
}

func TestFormatMessage_RevokedVariants(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := changeAt
	base := Message{ID: "f1", ChatJID: changeDM, Sender: "447700000001", Timestamp: changeAt, RevokedAt: &at}

	kept := base
	kept.Content = "still here"
	kept.EditedAt = &at
	if got := s.FormatMessage(ctx, kept, false); !strings.HasSuffix(got, "From: Alice: [deleted by sender] still here\n") {
		t.Errorf("keep-revoked line = %q", got)
	}
	media := base
	media.MediaType = "image"
	if got := s.FormatMessage(ctx, media, false); !strings.HasSuffix(got, "From: Alice: [deleted by sender] [image] \n") {
		t.Errorf("revoked media line = %q", got)
	}
}

// TestMigrateSchema_AddsEditRevokeColumns opens a database from before this
// change: Open must add edited_at and revoked_at, keep the rows, and stay
// idempotent.
func TestMigrateSchema_AddsEditRevokeColumns(t *testing.T) {
	dir := t.TempDir()
	legacy, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, direct_path TEXT, media_key BLOB, file_sha256 BLOB,
			file_enc_sha256 BLOB, file_length INTEGER, poll_options_json TEXT,
			quoted_message_id TEXT, quoted_sender TEXT, quoted_text TEXT, is_forwarded BOOLEAN DEFAULT 0,
			PRIMARY KEY (id, chat_jid)
		);
		INSERT INTO chats (jid, name) VALUES ('447700000001@s.whatsapp.net', 'Alice');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('old-1', '447700000001@s.whatsapp.net', '447700000001', 'before upgrade', '2026-01-01 09:00:00+00:00', 0);
		PRAGMA user_version = 1;
	`); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	_ = legacy.Close()

	for i := 0; i < 2; i++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		r := readChangeRow(t, s, "447700000001@s.whatsapp.net", "old-1")
		if r.content != "before upgrade" || r.edited.Valid || r.revoked.Valid {
			t.Errorf("Open #%d: row = %+v", i+1, r)
		}
		res, err := s.ApplyRevoke(context.Background(), MessageChange{
			ChatJID: "447700000001@s.whatsapp.net", ID: "old-1", ActorSenders: []string{"447700000001"}, At: changeAt,
		}, true)
		if err != nil || res != ChangeApplied {
			t.Errorf("Open #%d: ApplyRevoke on migrated row = %v, %v", i+1, res, err)
		}
		if _, err := s.db.Exec("UPDATE messages SET revoked_at = NULL"); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

// TestApplyEdit_IgnoresOlderEdit: edits replayed out of order (history lists
// newest first, and a sync can replay an edit already applied live) never
// roll the text back to an older version.
func TestApplyEdit_IgnoresOlderEdit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	storeChangeTarget(t, s, Message{ID: "o1", ChatJID: changeDM, Sender: "447700000001", Content: "v1"})
	ch := MessageChange{ChatJID: changeDM, ID: "o1", ActorSenders: []string{"447700000001"}}

	e1At, e2At := changeAt, changeAt.Add(time.Minute)
	ch.At = e2At
	if res, err := s.ApplyEdit(ctx, ch, "v3"); err != nil || res != ChangeApplied {
		t.Fatalf("newer edit = %v, %v; want applied", res, err)
	}
	ch.At = e1At
	if res, err := s.ApplyEdit(ctx, ch, "v2"); err != nil || res != ChangeStale {
		t.Fatalf("older edit = %v, %v; want stale", res, err)
	}
	if r := readChangeRow(t, s, changeDM, "o1"); r.content != "v3" || !r.edited.Time.Equal(e2At) {
		t.Errorf("after older edit: content=%q edited_at=%v, want v3 at %v", r.content, r.edited.Time, e2At)
	}
	// The same edit applied again is harmless.
	ch.At = e2At
	if res, err := s.ApplyEdit(ctx, ch, "v3"); err != nil || res != ChangeApplied {
		t.Errorf("replayed newest edit = %v, %v; want applied", res, err)
	}
}

// TestStoreMessage_EditRowOfOtherAuthor: a history edit whose original was
// not cached is stored as the row under the editor's sender. When the
// original arrives from a different author, it replaces that row; the edit
// of a group member who did not write the message never survives as the
// author's text.
func TestStoreMessage_EditRowOfOtherAuthor(t *testing.T) {
	s := openTestStore(t)
	const alice, bob = "447700000001", "447700000002"
	editAt := changeAt
	forged := Message{ID: "x1", ChatJID: changeGroup, Sender: bob, Content: "Bob war hier", EditedAt: &editAt,
		Timestamp: changeAt}
	storeChangeTarget(t, s, forged)

	original := Message{ID: "x1", ChatJID: changeGroup, Sender: alice, Content: "Alice schreibt"}
	storeChangeTarget(t, s, original)
	r := readChangeRow(t, s, changeGroup, "x1")
	if r.content != "Alice schreibt" || r.edited.Valid {
		t.Errorf("after original: content=%q edited=%v, want Alice's text unedited", r.content, r.edited.Valid)
	}
	var sender string
	if err := s.db.QueryRow("SELECT sender FROM messages WHERE id = 'x1'").Scan(&sender); err != nil || sender != alice {
		t.Errorf("sender = %q (err %v), want %s", sender, err, alice)
	}

	// Another edit by Bob, stored as a row of its own, cannot replace it.
	later := changeAt.Add(time.Minute)
	forged.EditedAt = &later
	storeChangeTarget(t, s, forged)
	if r := readChangeRow(t, s, changeGroup, "x1"); r.content != "Alice schreibt" || r.edited.Valid {
		t.Errorf("foreign edit replaced the row: content=%q edited=%v", r.content, r.edited.Valid)
	}

	// The author's own edit stored first is kept when her original follows.
	own := Message{ID: "x2", ChatJID: changeGroup, Sender: alice, Content: "korrigiert", EditedAt: &editAt, Timestamp: changeAt}
	storeChangeTarget(t, s, own)
	storeChangeTarget(t, s, Message{ID: "x2", ChatJID: changeGroup, Sender: alice, Content: "Tippfehler"})
	if r := readChangeRow(t, s, changeGroup, "x2"); r.content != "korrigiert" || !r.edited.Valid {
		t.Errorf("author's edit lost: content=%q edited=%v", r.content, r.edited.Valid)
	}
}

// TestRevokedOriginal_HidesQuotedCopy: a reply carries a copy of the text it
// quotes. Once that message is deleted by its sender, neither list_messages
// nor get_message_context may show the copy, also when the reply is stored
// again later (history sync).
func TestRevokedOriginal_HidesQuotedCopy(t *testing.T) {
	const secret = "Kontonummer DE00 1234"
	for _, keep := range []bool{false, true} {
		s := openTestStore(t)
		ctx := context.Background()
		storeChangeTarget(t, s, Message{ID: "t1", ChatJID: changeDM, Sender: "447700000001", Content: secret})
		storeChangeTarget(t, s, Message{ID: "t2", ChatJID: changeDM, Sender: "447700000001", MediaType: "image",
			Content: "Foto vom Vertrag", Timestamp: time.Date(2026, 3, 1, 9, 1, 0, 0, time.UTC)})
		reply := Message{ID: "r1", ChatJID: changeDM, IsFromMe: true, Sender: "me", Content: "danke",
			Timestamp:       time.Date(2026, 3, 1, 9, 5, 0, 0, time.UTC),
			QuotedMessageID: "t1", QuotedSender: "447700000001", QuotedText: secret}
		captionReply := Message{ID: "r2", ChatJID: changeDM, IsFromMe: true, Sender: "me", Content: "ok",
			Timestamp:       time.Date(2026, 3, 1, 9, 6, 0, 0, time.UTC),
			QuotedMessageID: "t2", QuotedSender: "447700000001", QuotedText: "[image] Foto vom Vertrag"}
		storeChangeTarget(t, s, reply)
		storeChangeTarget(t, s, captionReply)

		for _, id := range []string{"t1", "t2"} {
			ch := MessageChange{ChatJID: changeDM, ID: id, ActorSenders: []string{"447700000001"}, At: changeAt}
			if res, err := s.ApplyRevoke(ctx, ch, keep); err != nil || res != ChangeApplied {
				t.Fatalf("keep=%v: revoke %s = %v, %v", keep, id, res, err)
			}
		}

		check := func(stage string) {
			t.Helper()
			msgs, err := s.ListMessages(ctx, ListMessagesParams{ChatJID: changeDM, After: time.Date(2026, 3, 1, 9, 4, 0, 0, time.UTC)})
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			text := s.FormatMessagesList(ctx, msgs, false)
			mc, err := s.GetMessageContext(ctx, "r1", 0, 1)
			if err != nil {
				t.Fatalf("GetMessageContext: %v", err)
			}
			raw, _ := json.Marshal(mc)
			if !keep {
				for _, leak := range []string{secret, "Foto vom Vertrag"} {
					if strings.Contains(text, leak) || strings.Contains(string(raw), leak) {
						t.Errorf("%s: deleted text %q readable:\n%s\n%s", stage, leak, text, raw)
					}
				}
				if !strings.Contains(text, `(reply to t1 from Alice: "[deleted by sender]") danke`) ||
					!strings.Contains(text, `(reply to t2 from Alice: "[deleted by sender] [image]") ok`) {
					t.Errorf("%s: reply markers:\n%s", stage, text)
				}
				var stored sql.NullString
				if err := s.db.QueryRow("SELECT quoted_text FROM messages WHERE id = 'r1'").Scan(&stored); err != nil || stored.String != "" {
					t.Errorf("%s: quoted_text still stored: %q (err %v)", stage, stored.String, err)
				}
			} else if !strings.Contains(text, `"[deleted by sender] `+secret+`") danke`) {
				t.Errorf("%s: keep-revoked reply marker:\n%s", stage, text)
			}
		}
		check("after revoke")
		// History sync delivers the replies again, embedded copies included.
		storeChangeTarget(t, s, reply)
		storeChangeTarget(t, s, captionReply)
		check("after re-delivery")
	}
}

// TestApplyPendingRevokes: a revoke stored before its message is applied
// once the message is cached, with the usual author check. A revoke whose
// message is still missing stays pending; one that was applied or refused
// leaves the table.
func TestApplyPendingRevokes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, ch := range []MessageChange{
		{ChatJID: changeDM, ID: "p1", ActorSenders: []string{"447700000001"}, At: changeAt},
		{ChatJID: changeDM, ID: "p2", ActorSenders: []string{"447700000099"}, At: changeAt},
		{ChatJID: changeDM, ID: "p3", ActorSenders: []string{"447700000001"}, At: changeAt},
	} {
		if err := s.AddPendingRevoke(ctx, ch); err != nil {
			t.Fatalf("AddPendingRevoke %s: %v", ch.ID, err)
		}
	}
	// A second revoke of p1 keeps the first time.
	if err := s.AddPendingRevoke(ctx, MessageChange{ChatJID: changeDM, ID: "p1", ActorSenders: []string{"447700000001"}, At: changeAt.Add(time.Hour)}); err != nil {
		t.Fatalf("AddPendingRevoke again: %v", err)
	}

	storeChangeTarget(t, s, Message{ID: "p1", ChatJID: changeDM, Sender: "447700000001", Content: "gelöscht"})
	storeChangeTarget(t, s, Message{ID: "p2", ChatJID: changeDM, Sender: "447700000001", Content: "nicht von p2s Absender gelöscht"})

	// Only p1 when asked for p1.
	applied, err := s.ApplyPendingRevokes(ctx, changeDM, "p1", false)
	if err != nil || len(applied) != 1 || applied[0].ID != "p1" {
		t.Fatalf("ApplyPendingRevokes(p1) = %+v, %v", applied, err)
	}
	if r := readChangeRow(t, s, changeDM, "p1"); r.content != "" || !r.revoked.Valid || !r.revoked.Time.Equal(changeAt) {
		t.Errorf("p1 = %+v, want blanked with revoked_at %v", r, changeAt)
	}
	if r := readChangeRow(t, s, changeDM, "p2"); r.revoked.Valid {
		t.Errorf("p2 revoked before its pending revoke was applied: %+v", r)
	}

	// The whole chat: p2 is refused (other sender), p3 still missing.
	applied, err = s.ApplyPendingRevokes(ctx, changeDM, "", false)
	if err != nil || len(applied) != 0 {
		t.Fatalf("ApplyPendingRevokes(chat) = %+v, %v; want nothing applied", applied, err)
	}
	if r := readChangeRow(t, s, changeDM, "p2"); r.revoked.Valid || r.content == "" {
		t.Errorf("p2 = %+v, want untouched (revoke from another sender)", r)
	}
	var left []string
	rows, err := s.db.Query("SELECT message_id FROM pending_revokes ORDER BY message_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if strings.Join(left, ",") != "p3" {
		t.Errorf("pending left = %v, want only p3", left)
	}
}

// TestGetChat_PreviewMarksDeletedAndEdited: the last-message preview of
// list_chats/get_chat carries the same markers as a list_messages line.
func TestGetChat_PreviewMarksDeletedAndEdited(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	late := time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)
	storeChangeTarget(t, s, Message{ID: "pv", ChatJID: changeDM, Sender: "447700000001", Content: "Preis 500 Euro", Timestamp: late})

	preview := func() string {
		t.Helper()
		c, err := s.GetChat(ctx, changeDM, true)
		if err != nil || c == nil {
			t.Fatalf("GetChat = %v, %v", c, err)
		}
		return c.LastMessage
	}
	ch := MessageChange{ChatJID: changeDM, ID: "pv", ActorSenders: []string{"447700000001"}, At: late.Add(time.Minute)}
	if _, err := s.ApplyEdit(ctx, ch, "Preis 450 Euro"); err != nil {
		t.Fatal(err)
	}
	if got := preview(); got != "[edited] Preis 450 Euro" {
		t.Errorf("edited preview = %q", got)
	}
	ch.At = late.Add(2 * time.Minute)
	if _, err := s.ApplyRevoke(ctx, ch, true); err != nil {
		t.Fatal(err)
	}
	if got := preview(); got != "[deleted by sender] Preis 450 Euro" {
		t.Errorf("revoked preview with kept text = %q", got)
	}
	chats, err := s.ListChats(ctx, "447700000001", 10, 0, true, "")
	if err != nil || len(chats) == 0 || chats[0].LastMessage != "[deleted by sender] Preis 450 Euro" {
		t.Errorf("ListChats preview = %+v, %v", chats, err)
	}
}
