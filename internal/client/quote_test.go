package client

import (
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

func TestContextInfoOf(t *testing.T) {
	ci := &waProto.ContextInfo{StanzaID: proto.String("Q1")}
	cases := []struct {
		name string
		msg  *waProto.Message
	}{
		{"extended text", &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{ContextInfo: ci}}},
		{"image", &waProto.Message{ImageMessage: &waProto.ImageMessage{ContextInfo: ci}}},
		{"video", &waProto.Message{VideoMessage: &waProto.VideoMessage{ContextInfo: ci}}},
		{"document", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{ContextInfo: ci}}},
		{"audio", &waProto.Message{AudioMessage: &waProto.AudioMessage{ContextInfo: ci}}},
		{"sticker", &waProto.Message{StickerMessage: &waProto.StickerMessage{ContextInfo: ci}}},
	}
	for _, tc := range cases {
		if got := contextInfoOf(tc.msg); got.GetStanzaID() != "Q1" {
			t.Errorf("%s: contextInfoOf = %v, want the attached ContextInfo", tc.name, got)
		}
	}
	if got := contextInfoOf(nil); got != nil {
		t.Errorf("nil message: got %v", got)
	}
	if got := contextInfoOf(&waProto.Message{Conversation: proto.String("plain")}); got != nil {
		t.Errorf("plain conversation: got %v", got)
	}
}

func TestQuotedSnippet(t *testing.T) {
	cases := []struct {
		name string
		msg  *waProto.Message
		want string
	}{
		{"nil", nil, ""},
		{"text", &waProto.Message{Conversation: proto.String("are you free\ntomorrow?")}, "are you free tomorrow?"},
		{"extended text", &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("link inside")}}, "link inside"},
		{"image with caption", &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("the kitchen")}}, "[image] the kitchen"},
		{"video", &waProto.Message{VideoMessage: &waProto.VideoMessage{}}, "[video]"},
		{"voice note", &waProto.Message{AudioMessage: &waProto.AudioMessage{}}, "[audio]"},
		{"document", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String("Rechnung 12.pdf")}}, "[document: Rechnung 12.pdf]"},
		{"sticker", &waProto.Message{StickerMessage: &waProto.StickerMessage{}}, "[sticker]"},
		{"document with caption", &waProto.Message{DocumentWithCaptionMessage: &waProto.FutureProofMessage{
			Message: &waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String("Angebot.pdf"), Caption: proto.String("bitte prüfen")}},
		}}, "[document: Angebot.pdf] bitte prüfen"},
		{"view-once image", &waProto.Message{ViewOnceMessageV2: &waProto.FutureProofMessage{
			Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{}},
		}}, "[image]"},
		{"ephemeral text", &waProto.Message{EphemeralMessage: &waProto.FutureProofMessage{
			Message: &waProto.Message{Conversation: proto.String("kurz")},
		}}, "kurz"},
	}
	for _, tc := range cases {
		if got := quotedSnippet(tc.msg); got != tc.want {
			t.Errorf("%s: quotedSnippet = %q, want %q", tc.name, got, tc.want)
		}
	}
	long := &waProto.Message{Conversation: proto.String(strings.Repeat("x", 1000))}
	if got := quotedSnippet(long); len([]rune(got)) != 200 {
		t.Errorf("long quote has %d runes, want 200", len([]rune(got)))
	}
}

func TestNormalizeIncomingMessage_Reply(t *testing.T) {
	s := newTestStoreWithLIDMap(t, map[string]string{"99887766": "447700000002"})
	c := newClientWithStore(t, s)

	nm := c.normalizeIncomingMessage(
		"123456789@g.us",
		"447700000001@s.whatsapp.net",
		"447700000001",
		true,
		false,
		"",
		"reply-1",
		time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		&waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String("yes, 5 works"),
			ContextInfo: &waProto.ContextInfo{
				StanzaID:      proto.String("ORIG-1"),
				Participant:   proto.String("99887766:3@lid"),
				QuotedMessage: &waProto.Message{Conversation: proto.String("can we meet at 5?")},
			},
		}},
	)

	m := nm.msg
	if m.Content != "yes, 5 works" {
		t.Errorf("content = %q", m.Content)
	}
	if m.QuotedMessageID != "ORIG-1" || m.QuotedSender != "447700000002" || m.QuotedText != "can we meet at 5?" {
		t.Errorf("quote = id %q sender %q text %q, want ORIG-1 / LID-resolved 447700000002 / original text",
			m.QuotedMessageID, m.QuotedSender, m.QuotedText)
	}
	if m.IsForwarded {
		t.Error("reply must not be marked forwarded")
	}
}

func TestNormalizeIncomingMessage_Forwarded(t *testing.T) {
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithStore(t, s)

	nm := c.normalizeIncomingMessage(
		"447700000001@s.whatsapp.net",
		"447700000001@s.whatsapp.net",
		"447700000001",
		false,
		false,
		"",
		"fwd-1",
		time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		&waProto.Message{ImageMessage: &waProto.ImageMessage{
			Caption:     proto.String("look at this"),
			ContextInfo: &waProto.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(1)},
		}},
	)

	if !nm.msg.IsForwarded {
		t.Error("IsForwarded should be true")
	}
	if nm.msg.QuotedMessageID != "" || nm.msg.QuotedSender != "" || nm.msg.QuotedText != "" {
		t.Errorf("forward without quote must leave quote fields empty: %+v", nm.msg)
	}
}

// TestHandleHistorySync_StoresReplyContext checks that a reply arriving by
// history sync, inside an ephemeral wrapper, keeps its quote and forward
// fields like a live message does.
func TestHandleHistorySync_StoresReplyContext(t *testing.T) {
	const dmJID = "447700000001@s.whatsapp.net"
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithContacts(t, s, "491700000000", nil)

	reply := &waProto.Message{EphemeralMessage: &waProto.FutureProofMessage{
		Message: &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String("see below"),
			ContextInfo: &waProto.ContextInfo{
				StanzaID:      proto.String("ORIG-H"),
				Participant:   proto.String("491700000000@s.whatsapp.net"),
				QuotedMessage: &waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String("Angebot.pdf")}},
				IsForwarded:   proto.Bool(true),
			},
		}},
	}}
	c.handleHistorySync(&events.HistorySync{Data: &waProto.HistorySync{
		Conversations: []*waProto.Conversation{
			historyConversation(dmJID, historyWebMessage("hist-reply-1", false, "", reply)),
		},
	}})

	var quotedID, quotedSender, quotedText string
	var forwarded bool
	err := s.DB().QueryRow(
		"SELECT quoted_message_id, quoted_sender, quoted_text, is_forwarded FROM messages WHERE id = ?", "hist-reply-1",
	).Scan(&quotedID, &quotedSender, &quotedText, &forwarded)
	if err != nil {
		t.Fatalf("history reply not stored: %v", err)
	}
	// The quoted document is our own (491700000000 is the paired number).
	if quotedID != "ORIG-H" || quotedSender != store.QuotedSenderMe || quotedText != "[document: Angebot.pdf]" || !forwarded {
		t.Errorf("stored quote = id %q sender %q text %q forwarded %v", quotedID, quotedSender, quotedText, forwarded)
	}
}

// TestNormalizeIncomingMessage_QuotesOwnMessage: a reply to one of our own
// messages is marked with store.QuotedSenderMe whether the quote names our
// phone number, our LID or a LID that resolves to our number, so it reads
// "from Me" without the cached original. Someone else's message is not.
func TestNormalizeIncomingMessage_QuotesOwnMessage(t *testing.T) {
	const own = "491700000000"
	s := newTestStoreWithLIDMap(t, map[string]string{"55554444": own})
	c := newClientWithContacts(t, s, own, nil)
	c.wa.Store.LID = types.NewJID("11112222", types.HiddenUserServer)

	cases := map[string]string{
		own + ":7@s.whatsapp.net":     store.QuotedSenderMe,
		"11112222:3@lid":              store.QuotedSenderMe,
		"55554444@lid":                store.QuotedSenderMe,
		"447700000001@s.whatsapp.net": "447700000001",
		"":                            "",
	}
	for participant, want := range cases {
		nm := c.normalizeIncomingMessage(
			"447700000001@s.whatsapp.net", "447700000001@s.whatsapp.net", "447700000001",
			false, false, "", "reply-own", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
			&waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
				Text: proto.String("passt"),
				ContextInfo: &waProto.ContextInfo{
					StanzaID:      proto.String("ORIG-OWN"),
					Participant:   proto.String(participant),
					QuotedMessage: &waProto.Message{Conversation: proto.String("Angebot anbei")},
				},
			}},
		)
		if nm.msg.QuotedSender != want {
			t.Errorf("participant %q: QuotedSender = %q, want %q", participant, nm.msg.QuotedSender, want)
		}
	}
}
