package client

import (
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// historyConversation builds a one-message history-sync conversation.
func historyConversation(chatJID string, webMsg *waProto.WebMessageInfo) *waProto.Conversation {
	return &waProto.Conversation{
		ID:       proto.String(chatJID),
		Messages: []*waProto.HistorySyncMsg{{Message: webMsg}},
	}
}

// historyWebMessage builds a WebMessageInfo with the given key and payload.
func historyWebMessage(id string, fromMe bool, participant string, payload *waProto.Message) *waProto.WebMessageInfo {
	wm := &waProto.WebMessageInfo{
		Key: &waProto.MessageKey{
			ID:     proto.String(id),
			FromMe: proto.Bool(fromMe),
		},
		Message:          payload,
		MessageTimestamp: proto.Uint64(uint64(time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC).Unix())),
	}
	if participant != "" {
		wm.Participant = proto.String(participant)
	}
	return wm
}

// TestHandleHistorySync_UnwrapsWrappedMessages checks that history messages
// wrapped in ephemeral, view-once and document-with-caption containers are
// stored with their text and media, as live messages are. Before the fix they
// came through empty and were dropped.
func TestHandleHistorySync_UnwrapsWrappedMessages(t *testing.T) {
	const (
		own      = "491700000000"
		dmJID    = "447700000001@s.whatsapp.net"
		groupJID = "123456789@g.us"
		lidJID   = "99887766@lid"
	)
	s := newTestStoreWithLIDMap(t, map[string]string{"99887766": "447700000002"})
	c := newClientWithContacts(t, s, own, nil)
	// A stored group name keeps GetChatName away from the network.
	if err := s.StoreChat(groupJID, "Project Team", time.Now()); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	ephemeral := func(text string) *waProto.Message {
		return &waProto.Message{EphemeralMessage: &waProto.FutureProofMessage{
			Message: &waProto.Message{Conversation: proto.String(text)},
		}}
	}

	c.handleHistorySync(&events.HistorySync{Data: &waProto.HistorySync{
		Conversations: []*waProto.Conversation{
			historyConversation(dmJID, historyWebMessage("eph-1", false, "", ephemeral("disappearing hello"))),
			historyConversation(groupJID, historyWebMessage("doc-1", false, "447700000003@s.whatsapp.net",
				&waProto.Message{DocumentWithCaptionMessage: &waProto.FutureProofMessage{
					Message: &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
						FileName: proto.String("Angebot.pdf"),
						Mimetype: proto.String("application/pdf"),
						Caption:  proto.String("see attached"),
					}},
				}})),
			historyConversation(dmJID, historyWebMessage("vo-1", false, "",
				&waProto.Message{ViewOnceMessageV2: &waProto.FutureProofMessage{
					Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("look")}},
				}})),
			historyConversation(dmJID, historyWebMessage("mine-1", true, "", ephemeral("my reply"))),
			historyConversation(lidJID, historyWebMessage("lid-1", false, "", ephemeral("hello from lid"))),
			// No participant anywhere: ParseWebMessage fails and the key
			// fallback runs, which must still unwrap the payload.
			historyConversation(groupJID, historyWebMessage("fallback-1", false, "", ephemeral("no participant"))),
		},
	}})

	cases := []struct {
		id, chat, sender, content, mediaType, filename string
		fromMe                                         bool
	}{
		{"eph-1", dmJID, "447700000001", "disappearing hello", "", "", false},
		{"doc-1", groupJID, "447700000003", "see attached", "document", "Angebot.pdf", false},
		{"vo-1", dmJID, "447700000001", "look", "image", "image_unknown.jpg", false},
		{"mine-1", dmJID, own, "my reply", "", "", true},
		{"lid-1", "447700000002@s.whatsapp.net", "447700000002", "hello from lid", "", "", false},
		{"fallback-1", groupJID, "123456789", "no participant", "", "", false},
	}
	for _, tc := range cases {
		var chat, sender, content, mediaType, filename string
		var fromMe bool
		err := s.DB().QueryRow(
			"SELECT chat_jid, sender, content, COALESCE(media_type, ''), COALESCE(filename, ''), is_from_me FROM messages WHERE id = ?", tc.id,
		).Scan(&chat, &sender, &content, &mediaType, &filename, &fromMe)
		if err != nil {
			t.Errorf("%s: not stored: %v", tc.id, err)
			continue
		}
		if chat != tc.chat || sender != tc.sender || content != tc.content ||
			mediaType != tc.mediaType || filename != tc.filename || fromMe != tc.fromMe {
			t.Errorf("%s: got chat=%q sender=%q content=%q media=%q file=%q fromMe=%v, want chat=%q sender=%q content=%q media=%q file=%q fromMe=%v",
				tc.id, chat, sender, content, mediaType, filename, fromMe,
				tc.chat, tc.sender, tc.content, tc.mediaType, tc.filename, tc.fromMe)
		}
	}
}

// TestHandleHistorySync_OwnMessageInLIDChat: in an @lid DM, ParseWebMessage
// names our own LID as the sender of our messages. They must still be stored
// under our phone number, marked as ours.
func TestHandleHistorySync_OwnMessageInLIDChat(t *testing.T) {
	const own = "491700000000"
	s := newTestStoreWithLIDMap(t, map[string]string{"99887766": "447700000002"})
	c := newClientWithContacts(t, s, own, nil)
	// Our own LID is deliberately not in the LID map.
	c.wa.Store.LID = types.NewJID("55554444", types.HiddenUserServer)

	c.handleHistorySync(&events.HistorySync{Data: &waProto.HistorySync{
		Conversations: []*waProto.Conversation{
			historyConversation("99887766@lid", historyWebMessage("mine-lid", true, "", &waProto.Message{Conversation: proto.String("meine Antwort")})),
		},
	}})

	var chat, sender string
	var fromMe bool
	if err := s.DB().QueryRow("SELECT chat_jid, sender, is_from_me FROM messages WHERE id = 'mine-lid'").Scan(&chat, &sender, &fromMe); err != nil {
		t.Fatalf("not stored: %v", err)
	}
	if chat != "447700000002@s.whatsapp.net" || sender != own || !fromMe {
		t.Errorf("got chat=%q sender=%q fromMe=%v, want chat 447700000002@s.whatsapp.net, sender %s, fromMe", chat, sender, fromMe, own)
	}
}

// TestHandleHistorySync_SameSecondKeepsSendOrder: history lists newest first.
// Two messages from the same second (a picture, then its explanation) must
// keep their send order, which the (timestamp, rowid) ordering relies on.
func TestHandleHistorySync_SameSecondKeepsSendOrder(t *testing.T) {
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithContacts(t, s, "491700000000", nil)
	const dm = "447700000001@s.whatsapp.net"

	conv := &waProto.Conversation{
		ID: proto.String(dm),
		Messages: []*waProto.HistorySyncMsg{
			{Message: historyWebMessage("second", false, "", &waProto.Message{Conversation: proto.String("das ist der Fehler")})},
			{Message: historyWebMessage("first", false, "", &waProto.Message{ImageMessage: &waProto.ImageMessage{}})},
		},
	}
	c.handleHistorySync(&events.HistorySync{Data: &waProto.HistorySync{Conversations: []*waProto.Conversation{conv}}})

	rows, err := s.DB().Query("SELECT id FROM messages WHERE chat_jid = ? ORDER BY timestamp, rowid", dm)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 2 || ids[0] != "first" || ids[1] != "second" {
		t.Errorf("order = %v, want [first second]", ids)
	}
}

// TestHistoryMessageFromKey_UnwrapsPayload covers the fallback on its own:
// sender comes from the key, the payload is unwrapped.
func TestHistoryMessageFromKey_UnwrapsPayload(t *testing.T) {
	chat := types.NewJID("123456789", types.GroupServer)
	wm := historyWebMessage("k-1", false, "", &waProto.Message{EphemeralMessage: &waProto.FutureProofMessage{
		Message: &waProto.Message{Conversation: proto.String("inner")},
	}})
	wm.Key.Participant = proto.String("447700000003@s.whatsapp.net")

	hm := historyMessageFromKey(chat, "491700000000", wm)
	if hm.id != "k-1" || hm.senderUser != "447700000003" || hm.isFromMe {
		t.Errorf("key fields = %+v", hm)
	}
	if got := extractTextContent(hm.message); got != "inner" {
		t.Errorf("text = %q, want \"inner\"", got)
	}
}
