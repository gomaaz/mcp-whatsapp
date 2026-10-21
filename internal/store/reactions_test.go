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

const rxChat = "491700000077@s.whatsapp.net"

var rxTime = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// seedReactionChat stores a short conversation with a named customer.
func seedReactionChat(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.StoreChat(rxChat, "Karla Kundin", rxTime); err != nil {
		t.Fatal(err)
	}
	for i, m := range []Message{
		{ID: "rx-1", Sender: "491700000077", Content: "Ist das Angebot fertig?"},
		{ID: "rx-2", Sender: "491700000000", Content: "Ja, kommt heute", IsFromMe: true},
		{ID: "rx-3", Sender: "491700000077", Content: "Super, danke!"},
	} {
		m.ChatJID = rxChat
		m.Timestamp = rxTime.Add(time.Duration(i) * time.Minute)
		if err := s.StoreMessage(ctx, m, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func react(t *testing.T, s *Store, id, sender string, fromMe bool, emoji string, at time.Time) {
	t.Helper()
	if err := s.StoreReaction(context.Background(), ReactionUpdate{
		ChatJID: rxChat, MessageID: id, Sender: sender, IsFromMe: fromMe, Emoji: emoji, Timestamp: at,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreReaction_ReplaceRemoveAndReplay(t *testing.T) {
	s := openTestStore(t)
	seedReactionChat(t, s)
	ctx := context.Background()

	react(t, s, "rx-3", "491700000000", true, "👍", rxTime.Add(time.Hour))
	react(t, s, "rx-3", "491700000000", true, "❤️", rxTime.Add(2*time.Hour)) // changed
	react(t, s, "rx-3", "491700000000", true, "👍", rxTime.Add(time.Hour))    // older replay: ignored
	react(t, s, "rx-1", "491700000077", false, "🙏", rxTime.Add(time.Hour))
	react(t, s, "rx-1", "491700000077", false, "", rxTime.Add(3*time.Hour)) // removed
	react(t, s, "rx-1", "491700000077", false, "🙏", rxTime.Add(time.Hour))  // older replay: ignored

	mc, err := s.GetMessageContext(ctx, "rx-2", 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(mc.Message.Reactions) != 0 || len(mc.Before) != 1 || len(mc.Before[0].Reactions) != 0 {
		t.Errorf("unexpected reactions on rx-2/rx-1: %+v / %+v", mc.Message.Reactions, mc.Before)
	}
	if len(mc.After) != 1 || len(mc.After[0].Reactions) != 1 {
		t.Fatalf("rx-3 reactions = %+v", mc.After)
	}
	r := mc.After[0].Reactions[0]
	if r.Emoji != "❤️" || !r.IsFromMe || r.Sender != "491700000000" || !r.Timestamp.Equal(rxTime.Add(2*time.Hour)) {
		t.Errorf("rx-3 reaction = %+v", r)
	}

	raw, err := json.Marshal(mc.After[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reactions":[{"sender":"491700000000","is_from_me":true,"emoji":"❤️","timestamp":"2026-03-02T11:00:00Z"}]`) {
		t.Errorf("JSON = %s", raw)
	}
	raw, _ = json.Marshal(mc.Message)
	if strings.Contains(string(raw), "reactions") {
		t.Errorf("message without reactions carries the field: %s", raw)
	}
}

func TestStoreReaction_RequiresTargetAndSender(t *testing.T) {
	s := openTestStore(t)
	if err := s.StoreReaction(context.Background(), ReactionUpdate{ChatJID: rxChat, Sender: "x", Emoji: "👍"}); err == nil {
		t.Error("reaction without message ID accepted")
	}
}

func TestListMessages_FormatsReactions(t *testing.T) {
	s := openTestStore(t)
	seedReactionChat(t, s)
	ctx := context.Background()
	react(t, s, "rx-1", "491700000077", false, "👍", rxTime.Add(time.Hour))
	react(t, s, "rx-1", "491700000000", true, "❤️", rxTime.Add(2*time.Hour))

	for _, withContext := range []bool{false, true} {
		msgs, err := s.ListMessages(ctx, ListMessagesParams{ChatJID: rxChat, Query: "Angebot", IncludeContext: withContext})
		if err != nil {
			t.Fatal(err)
		}
		out := s.FormatMessagesList(ctx, msgs, false)
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "message_id=rx-1 ") {
				line = l
			}
		}
		if !strings.HasSuffix(line, "Ist das Angebot fertig? [reactions: 👍 Karla Kundin, ❤️ Me]") {
			t.Errorf("context=%v: rx-1 line = %q", withContext, line)
		}
		if strings.Count(out, "[reactions:") != 1 {
			t.Errorf("context=%v: reactions shown on other lines:\n%s", withContext, out)
		}
	}
}

func TestMigrateSchema_AddsReactionsTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	// A database from before the reactions table.
	legacy, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(schema); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	for i := 0; i < 2; i++ { // the second open must be a no-op
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := s.StoreReaction(context.Background(), ReactionUpdate{
			ChatJID: rxChat, MessageID: "m", Sender: "491700000077", Emoji: "👍", Timestamp: rxTime,
		}); err != nil {
			t.Fatalf("open %d: store reaction: %v", i, err)
		}
		_ = s.Close()
	}
}

func TestStoreUndecryptable_NeverReplacesAndIsDeletable(t *testing.T) {
	s := openTestStore(t)
	seedReactionChat(t, s)
	ctx := context.Background()

	// An existing decrypted row stays as it is.
	if err := s.StoreUndecryptable(ctx, Message{ID: "rx-1", ChatJID: rxChat, Sender: "491700000077", Timestamp: rxTime}); err != nil {
		t.Fatal(err)
	}
	var content string
	_ = s.db.QueryRow("SELECT content FROM messages WHERE id = 'rx-1'").Scan(&content)
	if content != "Ist das Angebot fertig?" {
		t.Errorf("placeholder replaced a decrypted message: %q", content)
	}
	if err := s.DeleteUndecryptable(ctx, rxChat, "rx-1"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 'rx-1'").Scan(&n)
	if n != 1 {
		t.Error("DeleteUndecryptable removed a real message")
	}

	if err := s.StoreUndecryptable(ctx, Message{ID: "rx-u", ChatJID: rxChat, Sender: "491700000077", Timestamp: rxTime.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(ctx, ListMessagesParams{ChatJID: rxChat, Limit: 1})
	if err != nil || len(msgs) != 1 || msgs[0].ID != "rx-u" || msgs[0].Content != UndecryptablePlaceholder {
		t.Fatalf("newest message = %+v (err %v)", msgs, err)
	}
	if err := s.DeleteUndecryptable(ctx, rxChat, "rx-u"); err != nil {
		t.Fatal(err)
	}
	_ = s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 'rx-u'").Scan(&n)
	if n != 0 {
		t.Error("placeholder not deleted")
	}
}

func TestListUnanswered_PlaceholdersAndReactionsCountAsReplies(t *testing.T) {
	ctx := context.Background()
	since := rxTime.Add(-time.Hour)
	waiting := func(t *testing.T, s *Store) *UnansweredChat {
		t.Helper()
		got, err := s.ListUnanswered(ctx, since, false, 20)
		if err != nil {
			t.Fatal(err)
		}
		for i := range got {
			if got[i].ChatJID == rxChat {
				return &got[i]
			}
		}
		return nil
	}

	t.Run("waiting without reply", func(t *testing.T) {
		s := openTestStore(t)
		seedReactionChat(t, s)
		c := waiting(t, s)
		if c == nil || c.WaitingCount != 1 || c.LastMessage.ID != "rx-3" {
			t.Fatalf("waiting = %+v", c)
		}
	})

	t.Run("own sticker", func(t *testing.T) {
		s := openTestStore(t)
		seedReactionChat(t, s)
		if err := s.StoreMessage(ctx, Message{ID: "st", ChatJID: rxChat, Sender: "491700000000", IsFromMe: true,
			MediaType: "sticker", Filename: "sticker_x.webp", Timestamp: rxTime.Add(time.Hour)}, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
		if c := waiting(t, s); c != nil {
			t.Errorf("chat still waiting after own sticker: %+v", c)
		}
	})

	t.Run("own location", func(t *testing.T) {
		s := openTestStore(t)
		seedReactionChat(t, s)
		if err := s.StoreMessage(ctx, Message{ID: "loc", ChatJID: rxChat, Sender: "491700000000", IsFromMe: true,
			Content: "[location] Büro (52.5,13.4) https://maps.google.com/?q=52.5,13.4", Timestamp: rxTime.Add(time.Hour)}, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
		if c := waiting(t, s); c != nil {
			t.Errorf("chat still waiting after own location: %+v", c)
		}
	})

	t.Run("own reaction to the waiting message", func(t *testing.T) {
		s := openTestStore(t)
		seedReactionChat(t, s)
		react(t, s, "rx-3", "491700000000", true, "👍", rxTime.Add(time.Hour))
		if c := waiting(t, s); c != nil {
			t.Errorf("chat still waiting after own reaction: %+v", c)
		}
	})

	t.Run("own reaction to an older message", func(t *testing.T) {
		s := openTestStore(t)
		seedReactionChat(t, s)
		if err := s.StoreMessage(ctx, Message{ID: "rx-4", ChatJID: rxChat, Sender: "491700000077",
			Content: "Und noch eine Frage", Timestamp: rxTime.Add(30 * time.Minute)}, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
		react(t, s, "rx-3", "491700000000", true, "👍", rxTime.Add(time.Hour))
		c := waiting(t, s)
		if c == nil || c.WaitingCount != 1 || c.LastMessage.ID != "rx-4" {
			t.Fatalf("waiting = %+v, want only rx-4", c)
		}
		if c.LastReplyAt == nil || !c.LastReplyAt.Equal(rxTime.Add(time.Hour)) {
			t.Errorf("last_reply_at = %v, want the reaction time", c.LastReplyAt)
		}
	})

	t.Run("removed or foreign reactions", func(t *testing.T) {
		s := openTestStore(t)
		seedReactionChat(t, s)
		react(t, s, "rx-3", "491700000000", true, "👍", rxTime.Add(time.Hour))
		react(t, s, "rx-3", "491700000000", true, "", rxTime.Add(2*time.Hour))
		react(t, s, "rx-3", "491700000077", false, "❤️", rxTime.Add(time.Hour))
		if c := waiting(t, s); c == nil || c.WaitingCount != 1 {
			t.Errorf("waiting = %+v, want rx-3 still waiting", c)
		}
	})
}
