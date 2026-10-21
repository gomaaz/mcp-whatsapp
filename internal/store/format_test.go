package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFormatMessage_FromMe(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	m := Message{
		ID:        "m1",
		ChatJID:   "447700000001@s.whatsapp.net",
		Sender:    "me",
		Content:   "hello",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		IsFromMe:  true,
		ChatName:  "Alice",
	}
	got := s.FormatMessage(ctx, m, false)
	if !strings.Contains(got, "From: Me:") {
		t.Fatalf("expected 'From: Me:' in output, got %q", got)
	}
	if !strings.HasPrefix(got, "[2026-01-02T03:04:05Z] [message_id=m1 chat_jid=447700000001@s.whatsapp.net] ") {
		t.Fatalf("expected timestamp and ID prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("expected trailing newline, got %q", got)
	}
}

func TestFormatMessage_KnownSender(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	m := Message{
		ID:        "m2",
		ChatJID:   "447700000001@s.whatsapp.net",
		Sender:    "447700000001@s.whatsapp.net", // Alice, per seed data
		Content:   "hi",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		IsFromMe:  false,
	}
	got := s.FormatMessage(ctx, m, false)
	if !strings.Contains(got, "From: Alice:") {
		t.Fatalf("expected 'From: Alice:' in output, got %q", got)
	}
}

func TestFormatMessage_MediaPrefix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	m := Message{
		ID:        "m3",
		ChatJID:   "447700000001@s.whatsapp.net",
		Sender:    "me",
		Content:   "caption",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		IsFromMe:  true,
		MediaType: "image",
	}
	got := s.FormatMessage(ctx, m, false)
	want := "[2026-01-02T03:04:05Z] [message_id=m3 chat_jid=447700000001@s.whatsapp.net] From: Me: [image] caption\n"
	if got != want {
		t.Fatalf("FormatMessage media:\n got %q\nwant %q", got, want)
	}
}

// Text messages carry the same ID tag as media messages, so any line can be
// handed to get_message_context.
func TestFormatMessage_TextLineCarriesIDs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	m := Message{
		ID:        "m5",
		ChatJID:   "447700000001@s.whatsapp.net",
		Sender:    "447700000001@s.whatsapp.net", // Alice, per seed data
		Content:   "plain text",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ChatName:  "Alice",
	}
	got := s.FormatMessage(ctx, m, true)
	want := "[2026-01-02T03:04:05Z] [message_id=m5 chat_jid=447700000001@s.whatsapp.net] Chat: Alice From: Alice: plain text\n"
	if got != want {
		t.Fatalf("FormatMessage text:\n got %q\nwant %q", got, want)
	}
}

func TestFormatMessage_NoChatPrefixWhenNotShown(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	m := Message{
		ID:        "m4",
		ChatJID:   "447700000001@s.whatsapp.net",
		Sender:    "me",
		Content:   "hi",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		IsFromMe:  true,
		ChatName:  "Alice",
	}
	got := s.FormatMessage(ctx, m, false)
	if strings.Contains(got, "Chat: ") {
		t.Fatalf("did not expect 'Chat:' prefix when showChatInfo=false, got %q", got)
	}

	withChat := s.FormatMessage(ctx, m, true)
	if !strings.Contains(withChat, "Chat: Alice") {
		t.Fatalf("expected 'Chat: Alice' when showChatInfo=true, got %q", withChat)
	}
}

func TestFormatMessagesList_Empty(t *testing.T) {
	s := openTestStore(t)
	got := s.FormatMessagesList(context.Background(), nil, false)
	if got != "No messages to display." {
		t.Fatalf("expected empty-message placeholder, got %q", got)
	}
}

func TestFormatMessagesList_NonEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	msgs := []Message{
		{
			ID:        "x1",
			ChatJID:   "447700000001@s.whatsapp.net",
			Sender:    "me",
			Content:   "one",
			Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			IsFromMe:  true,
		},
		{
			ID:        "x2",
			ChatJID:   "447700000001@s.whatsapp.net",
			Sender:    "me",
			Content:   "two",
			Timestamp: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
			IsFromMe:  true,
		},
	}
	got := s.FormatMessagesList(ctx, msgs, false)
	// Two messages => two newlines.
	if n := strings.Count(got, "\n"); n != 2 {
		t.Fatalf("expected 2 newlines, got %d in %q", n, got)
	}
	if !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf("expected both message contents, got %q", got)
	}
}
