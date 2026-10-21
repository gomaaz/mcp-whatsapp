package store

import (
	"context"
	"testing"
	"time"
)

// seedUnanswered adds chats on top of the seed fixtures for the
// ListUnanswered tests. Every row is after 2026-02-01, so a since of
// 2026-02-01 hides the older seed messages.
func seedUnanswered(t *testing.T, s *Store) {
	t.Helper()
	stmts := []string{
		`INSERT INTO chats (jid, name, last_message_time) VALUES
			('491700000001@s.whatsapp.net', '491700000001', '2026-02-03 10:00:00+00:00'),
			('491700000002@s.whatsapp.net', 'Answered',     '2026-02-03 11:00:00+00:00'),
			('491700000003@s.whatsapp.net', 'Customer C',   '2026-02-02 09:00:00+00:00'),
			('987654321@g.us',              'Club',         '2026-02-03 12:00:00+00:00'),
			('status@broadcast',            '',             '2026-02-01 08:00:00+00:00'),
			('1234567890@broadcast',        'List',         '2026-02-01 08:00:00+00:00'),
			('491700000004@s.whatsapp.net', 'Deleted',      '2026-02-02 09:00:00+00:00')`,
		`INSERT INTO whatsmeow_contacts (our_jid, their_jid, first_name, full_name, push_name, business_name) VALUES
			('me@s.whatsapp.net', '491700000001@s.whatsapp.net', 'Erika', 'Erika Mustermann', 'Erika', '')`,
		`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, revoked_at) VALUES
			-- Waiting chat: replied on 02-02, then three incoming messages,
			-- the last two in the same second (rowid decides the newest).
			('w0', '491700000001@s.whatsapp.net', '491700000001', 'first question', '2026-02-01 09:00:00+00:00', 0, '', '', NULL),
			('w1', '491700000001@s.whatsapp.net', 'me',           'my reply',       '2026-02-02 09:00:00+00:00', 1, '', '', NULL),
			('w2', '491700000001@s.whatsapp.net', '491700000001', 'follow-up',      '2026-02-02 10:00:00+00:00', 0, '', '', NULL),
			('w3', '491700000001@s.whatsapp.net', '491700000001', 'photo',          '2026-02-03 10:00:00+00:00', 0, 'image', 'image_x.jpg', NULL),
			('w4', '491700000001@s.whatsapp.net', '491700000001', 'see the photo?', '2026-02-03 10:00:00+00:00', 0, '', '', NULL),
			-- Signal-only row: neither text nor media; must be ignored.
			('w5', '491700000001@s.whatsapp.net', '491700000001', '',               '2026-02-04 10:00:00+00:00', 0, '', '', NULL),

			-- Answered chat: our reply is the newest message.
			('x1', '491700000002@s.whatsapp.net', '491700000002', 'question',       '2026-02-03 10:00:00+00:00', 0, '', '', NULL),
			('x2', '491700000002@s.whatsapp.net', 'me',           'answer',         '2026-02-03 11:00:00+00:00', 1, '', '', NULL),

			-- Never replied; waiting longer than the chat above.
			('c1', '491700000003@s.whatsapp.net', '491700000003', 'hello?',         '2026-02-01 12:00:00+00:00', 0, '', '', NULL),
			('c2', '491700000003@s.whatsapp.net', '491700000003', '',               '2026-02-02 09:00:00+00:00', 0, 'document', 'offer.pdf', NULL),

			-- Group with an unanswered message.
			('g9', '987654321@g.us',              '491700000009', 'who is coming?', '2026-02-03 12:00:00+00:00', 0, '', '', NULL),

			-- Status update and broadcast list: never listed.
			('s1', 'status@broadcast',            '491700000001', 'my status',      '2026-02-01 08:00:00+00:00', 0, 'image', 'st.jpg', NULL),
			('l1', '1234567890@broadcast',        '491700000002', 'newsletter',     '2026-02-01 08:00:00+00:00', 0, '', '', NULL),

			-- Only incoming message was deleted by its sender.
			('d1', '491700000004@s.whatsapp.net', '491700000004', '',               '2026-02-02 09:00:00+00:00', 0, '', '', '2026-02-02 09:01:00+00:00')`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("seed unanswered: %v", err)
		}
	}
}

func unansweredByJID(list []UnansweredChat) map[string]UnansweredChat {
	out := make(map[string]UnansweredChat, len(list))
	for _, c := range list {
		out[c.ChatJID] = c
	}
	return out
}

func TestListUnanswered_WaitingChats(t *testing.T) {
	s := openTestStore(t)
	seedUnanswered(t, s)
	since := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	got, err := s.ListUnanswered(context.Background(), since, false, 20)
	if err != nil {
		t.Fatalf("ListUnanswered: %v", err)
	}
	var jids []string
	for _, c := range got {
		jids = append(jids, c.ChatJID)
	}
	want := []string{"491700000003@s.whatsapp.net", "491700000001@s.whatsapp.net"}
	if len(jids) != len(want) || jids[0] != want[0] || jids[1] != want[1] {
		t.Fatalf("chats = %v, want %v (longest wait first)", jids, want)
	}

	w := got[1]
	if w.WaitingCount != 3 {
		t.Errorf("waiting_count = %d, want 3 (w2, w3, w4)", w.WaitingCount)
	}
	if want := time.Date(2026, 2, 2, 10, 0, 0, 0, time.UTC); !w.FirstWaitingAt.Equal(want) {
		t.Errorf("first_waiting_at = %v, want %v", w.FirstWaitingAt, want)
	}
	if want := time.Date(2026, 2, 3, 10, 0, 0, 0, time.UTC); !w.LastWaitingAt.Equal(want) {
		t.Errorf("last_waiting_at = %v, want %v", w.LastWaitingAt, want)
	}
	if w.LastReplyAt == nil || !w.LastReplyAt.Equal(time.Date(2026, 2, 2, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("last_reply_at = %v, want 2026-02-02 09:00 UTC", w.LastReplyAt)
	}
	if w.Name != "Erika Mustermann" {
		t.Errorf("name = %q, want the address-book name", w.Name)
	}
	if w.PhoneNumber != "491700000001" || w.IsGroup {
		t.Errorf("phone = %q, is_group = %v", w.PhoneNumber, w.IsGroup)
	}
	if lm := w.LastMessage; lm.ID != "w4" || lm.Text != "see the photo?" || lm.MediaType != "" || lm.Caption != "" || lm.Sender != "491700000001" {
		t.Errorf("last_message = %+v, want w4 text from 491700000001", lm)
	}

	c := got[0]
	if c.WaitingCount != 2 || c.LastReplyAt != nil {
		t.Errorf("never-replied chat: waiting_count = %d, last_reply_at = %v", c.WaitingCount, c.LastReplyAt)
	}
	if lm := c.LastMessage; lm.ID != "c2" || lm.MediaType != "document" || lm.Filename != "offer.pdf" || lm.Text != "" {
		t.Errorf("last_message = %+v, want document c2", lm)
	}
}

func TestListUnanswered_MediaCaption(t *testing.T) {
	s := openTestStore(t)
	seedUnanswered(t, s)
	if _, err := s.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename)
		VALUES ('w6', '491700000001@s.whatsapp.net', '491700000001', 'our invoice', '2026-02-05 10:00:00+00:00', 0, 'image', 'i.jpg')`); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListUnanswered(context.Background(), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), false, 20)
	if err != nil {
		t.Fatalf("ListUnanswered: %v", err)
	}
	w, ok := unansweredByJID(got)["491700000001@s.whatsapp.net"]
	if !ok {
		t.Fatal("waiting chat missing")
	}
	if lm := w.LastMessage; lm.ID != "w6" || lm.MediaType != "image" || lm.Caption != "our invoice" || lm.Text != "" {
		t.Errorf("last_message = %+v, want image w6 with caption", lm)
	}
}

func TestListUnanswered_Exclusions(t *testing.T) {
	s := openTestStore(t)
	seedUnanswered(t, s)
	since := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	for _, includeGroups := range []bool{false, true} {
		got, err := s.ListUnanswered(context.Background(), since, includeGroups, 20)
		if err != nil {
			t.Fatalf("ListUnanswered(includeGroups=%v): %v", includeGroups, err)
		}
		m := unansweredByJID(got)
		for _, jid := range []string{
			"491700000002@s.whatsapp.net", // answered
			"status@broadcast",
			"1234567890@broadcast",
			"491700000004@s.whatsapp.net", // only message revoked
		} {
			if _, ok := m[jid]; ok {
				t.Errorf("includeGroups=%v: %s listed, want excluded", includeGroups, jid)
			}
		}
		g, ok := m["987654321@g.us"]
		if ok != includeGroups {
			t.Errorf("includeGroups=%v: group listed = %v", includeGroups, ok)
		}
		if ok && (!g.IsGroup || g.WaitingCount != 1 || g.Name != "Club" || g.PhoneNumber != "" || g.LastMessage.Sender != "491700000009") {
			t.Errorf("group entry = %+v", g)
		}
	}
}

func TestListUnanswered_SinceAndLimit(t *testing.T) {
	s := openTestStore(t)
	seedUnanswered(t, s)
	ctx := context.Background()

	// since after the follow-ups of the waiting chat: only w3/w4 count.
	got, err := s.ListUnanswered(ctx, time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC), false, 20)
	if err != nil {
		t.Fatalf("ListUnanswered: %v", err)
	}
	if len(got) != 1 || got[0].ChatJID != "491700000001@s.whatsapp.net" || got[0].WaitingCount != 2 {
		t.Fatalf("since 02-03: got %+v, want only the waiting chat with 2 messages", got)
	}

	got, err = s.ListUnanswered(ctx, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), true, 1)
	if err != nil {
		t.Fatalf("ListUnanswered: %v", err)
	}
	if len(got) != 1 || got[0].ChatJID != "491700000003@s.whatsapp.net" {
		t.Fatalf("limit 1: got %+v, want the longest-waiting chat", got)
	}

	// Zero since: the older seed chats appear too (Bob's b3 after b2, the
	// seed group's g3/g4 after g2, Alice's a5 after a4).
	got, err = s.ListUnanswered(ctx, time.Time{}, true, 50)
	if err != nil {
		t.Fatalf("ListUnanswered: %v", err)
	}
	m := unansweredByJID(got)
	if c, ok := m["123456789@g.us"]; !ok || c.WaitingCount != 2 {
		t.Errorf("seed group: %+v, want 2 waiting (g3, g4)", c)
	}
	if c, ok := m["447700000002@s.whatsapp.net"]; !ok || c.WaitingCount != 1 || c.LastMessage.ID != "b3" {
		t.Errorf("Bob: %+v, want b3 waiting", c)
	}
}

func TestListUnanswered_RevokedReplyDoesNotCount(t *testing.T) {
	s := openTestStore(t)
	seedUnanswered(t, s)
	// Our reply in the answered chat is deleted for everyone: the customer
	// is waiting again.
	if _, err := s.db.Exec(`UPDATE messages SET revoked_at = '2026-02-03 11:05:00+00:00', content = '' WHERE id = 'x2'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListUnanswered(context.Background(), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), false, 20)
	if err != nil {
		t.Fatalf("ListUnanswered: %v", err)
	}
	c, ok := unansweredByJID(got)["491700000002@s.whatsapp.net"]
	if !ok || c.WaitingCount != 1 || c.LastMessage.ID != "x1" || c.LastReplyAt != nil {
		t.Errorf("chat with revoked reply: %+v (listed %v), want x1 waiting and no last_reply_at", c, ok)
	}
}
