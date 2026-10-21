package client

import (
	"database/sql"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

const (
	chgDM    = "447700000001@s.whatsapp.net"
	chgGroup = "123456789@g.us"
)

var chgTime = time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)

// liveMessage builds a live message event in chat from sender.
func liveMessage(t *testing.T, chat, sender, id string, fromMe bool, ts time.Time, m *waProto.Message) *events.Message {
	t.Helper()
	chatJID, err := types.ParseJID(chat)
	if err != nil {
		t.Fatal(err)
	}
	senderJID, err := types.ParseJID(sender)
	if err != nil {
		t.Fatal(err)
	}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chatJID, Sender: senderJID, IsFromMe: fromMe, IsGroup: chatJID.Server == types.GroupServer},
			ID:            id,
			Timestamp:     ts,
		},
		Message: m,
	}
}

// editPayload is the unwrapped form of a live edit: a MESSAGE_EDIT protocol
// message carrying the new content for target.
func editPayload(target string, edited *waProto.Message) *waProto.Message {
	return &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type:          waProto.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waProto.MessageKey{ID: proto.String(target)},
		EditedMessage: edited,
	}}
}

func revokePayload(target string) *waProto.Message {
	return &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type: waProto.ProtocolMessage_REVOKE.Enum(),
		Key:  &waProto.MessageKey{ID: proto.String(target)},
	}}
}

func text(s string) *waProto.Message { return &waProto.Message{Conversation: proto.String(s)} }

type chgRow struct {
	content         string
	edited, revoked bool
	mediaKeyPresent bool
}

func readChg(t *testing.T, s *store.Store, chat, id string) chgRow {
	t.Helper()
	var r chgRow
	var edited, revoked sql.NullTime
	var key []byte
	err := s.DB().QueryRow("SELECT content, edited_at, revoked_at, media_key FROM messages WHERE id = ? AND chat_jid = ?", id, chat).
		Scan(&r.content, &edited, &revoked, &key)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	r.edited, r.revoked, r.mediaKeyPresent = edited.Valid, revoked.Valid, key != nil
	return r
}

func newChangeClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	s := newTestStoreWithLIDMap(t, map[string]string{"99887766": "447700000002"})
	c := newClientWithStore(t, s)
	// Stored names keep GetChatName away from the (nil) whatsmeow client.
	for jid, name := range map[string]string{chgDM: "Alice", chgGroup: "Project Team"} {
		if err := s.StoreChat(jid, name, chgTime); err != nil {
			t.Fatal(err)
		}
	}
	return c, s
}

func TestHandleMessage_LiveEditUpdatesText(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleMessage(liveMessage(t, chgDM, chgDM, "orig-1", false, chgTime, text("Termin am Dienstag")))
	c.handleMessage(liveMessage(t, chgDM, chgDM, "edit-1", false, chgTime.Add(time.Minute),
		editPayload("orig-1", &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("Termin am Mittwoch")}})))

	if r := readChg(t, s, chgDM, "orig-1"); r.content != "Termin am Mittwoch" || !r.edited || r.revoked {
		t.Errorf("after edit: %+v", r)
	}
	var n int
	_ = s.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE id = 'edit-1'").Scan(&n)
	if n != 0 {
		t.Errorf("the edit itself was stored as a message")
	}
}

func TestHandleMessage_LiveCaptionEdit(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleMessage(liveMessage(t, chgDM, chgDM, "img-1", false, chgTime,
		&waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Foto alt"), MediaKey: []byte{9}}}))
	c.handleMessage(liveMessage(t, chgDM, chgDM, "edit-2", false, chgTime.Add(time.Minute),
		editPayload("img-1", &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Foto neu")}})))
	if r := readChg(t, s, chgDM, "img-1"); r.content != "Foto neu" || !r.edited || !r.mediaKeyPresent {
		t.Errorf("caption edit: %+v (media key must survive)", r)
	}
}

func TestHandleMessage_EditFromOtherSenderIgnored(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleMessage(liveMessage(t, chgGroup, "447700000001@s.whatsapp.net", "g-1", false, chgTime, text("Alice schreibt")))
	// Bob (via his LID) tries to edit and delete Alice's message.
	c.handleMessage(liveMessage(t, chgGroup, "99887766@lid", "g-edit", false, chgTime.Add(time.Minute), editPayload("g-1", text("Bob war hier"))))
	c.handleMessage(liveMessage(t, chgGroup, "99887766@lid", "g-del", false, chgTime.Add(time.Minute), revokePayload("g-1")))
	if r := readChg(t, s, chgGroup, "g-1"); r.content != "Alice schreibt" || r.edited || r.revoked {
		t.Errorf("foreign change applied: %+v", r)
	}

	// Bob may change his own message, even when it arrives under his LID.
	c.handleMessage(liveMessage(t, chgGroup, "99887766@lid", "g-2", false, chgTime, text("Bob schreibt")))
	c.handleMessage(liveMessage(t, chgGroup, "99887766@lid", "g-edit2", false, chgTime.Add(time.Minute), editPayload("g-2", text("Bob korrigiert"))))
	if r := readChg(t, s, chgGroup, "g-2"); r.content != "Bob korrigiert" || !r.edited {
		t.Errorf("author edit not applied: %+v", r)
	}
}

func TestHandleMessage_OwnMessageOnlyFromOwnDevice(t *testing.T) {
	c, s := newChangeClient(t)
	const own = "491700000000@s.whatsapp.net"
	c.handleMessage(liveMessage(t, chgDM, own, "mine-1", true, chgTime, text("Angebot: 500 EUR")))
	// The contact cannot rewrite what we sent.
	c.handleMessage(liveMessage(t, chgDM, chgDM, "x-edit", false, chgTime.Add(time.Minute), editPayload("mine-1", text("Angebot: 5 EUR"))))
	if r := readChg(t, s, chgDM, "mine-1"); r.content != "Angebot: 500 EUR" || r.edited {
		t.Errorf("contact edited our message: %+v", r)
	}
	// Our other device can.
	c.handleMessage(liveMessage(t, chgDM, own, "own-edit", true, chgTime.Add(time.Minute), editPayload("mine-1", text("Angebot: 450 EUR"))))
	if r := readChg(t, s, chgDM, "mine-1"); r.content != "Angebot: 450 EUR" || !r.edited {
		t.Errorf("own edit not applied: %+v", r)
	}
}

func TestHandleMessage_LiveRevoke(t *testing.T) {
	for _, keep := range []bool{false, true} {
		c, s := newChangeClient(t)
		c.keepRevoked = keep
		c.handleMessage(liveMessage(t, chgDM, chgDM, "del-1", false, chgTime,
			&waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String("Rechnung.pdf"), Caption: proto.String("anbei"), MediaKey: []byte{7}}}))
		c.handleMessage(liveMessage(t, chgDM, chgDM, "rev-1", false, chgTime.Add(time.Minute), revokePayload("del-1")))

		r := readChg(t, s, chgDM, "del-1")
		if !r.revoked {
			t.Errorf("keep=%v: revoked_at not set", keep)
		}
		wantContent, wantKey := "", false
		if keep {
			wantContent, wantKey = "anbei", true
		}
		if r.content != wantContent || r.mediaKeyPresent != wantKey {
			t.Errorf("keep=%v: content=%q media_key=%v, want %q/%v", keep, r.content, r.mediaKeyPresent, wantContent, wantKey)
		}
	}
}

func TestHandleMessage_EditBeforeOriginalStoresNothing(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleMessage(liveMessage(t, chgDM, chgDM, "early-edit", false, chgTime, editPayload("later-1", text("edited first"))))
	c.handleMessage(liveMessage(t, chgDM, chgDM, "early-rev", false, chgTime, revokePayload("later-2")))
	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after orphan edit/revoke = %d (err %v), want 0", n, err)
	}
}

func TestEditedText(t *testing.T) {
	cases := []struct {
		name string
		m    *waProto.Message
		want string
		ok   bool
	}{
		{"nil", nil, "", false},
		{"text", text("neu"), "neu", true},
		{"caption removed", &waProto.Message{VideoMessage: &waProto.VideoMessage{}}, "", true},
		{"unreadable", &waProto.Message{PollCreationMessage: &waProto.PollCreationMessage{}}, "", false},
	}
	for _, tc := range cases {
		if got, ok := editedText(tc.m); got != tc.want || ok != tc.ok {
			t.Errorf("%s: editedText = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// REVOKE is the zero value of the protocol type enum: a message without a
// protocol part must never read as a revoke.
func TestChangeDetection_IgnoresNonProtocolMessages(t *testing.T) {
	if isChangeProtocol(nil) {
		t.Error("isChangeProtocol(nil) = true")
	}
	if isChangeProtocol((&waProto.Message{Conversation: proto.String("hi")}).GetProtocolMessage()) {
		t.Error("plain text detected as a change")
	}
	if !isChangeProtocol(revokePayload("x").GetProtocolMessage()) || !isChangeProtocol(editPayload("x", text("y")).GetProtocolMessage()) {
		t.Error("revoke/edit not detected")
	}
	hm := historyMessage{id: "m", message: text("hi")}
	if got := historyRevokeTarget(historyWebMessage("m", false, "", text("hi")), hm); got != "" {
		t.Errorf("historyRevokeTarget of plain text = %q", got)
	}
	hm.message = nil
	if got := historyRevokeTarget(historyWebMessage("m", false, "", nil), hm); got != "" {
		t.Errorf("historyRevokeTarget of empty entry = %q", got)
	}
}

// historyWebMessageAt is historyWebMessage with its own timestamp.
func historyWebMessageAt(id string, fromMe bool, ts time.Time, payload *waProto.Message) *waProto.WebMessageInfo {
	wm := historyWebMessage(id, fromMe, "", payload)
	wm.MessageTimestamp = proto.Uint64(uint64(ts.Unix()))
	return wm
}

func TestHandleHistorySync_AppliesRevokesAndEdits(t *testing.T) {
	const own = "491700000000"
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithContacts(t, s, own, nil)
	if err := s.StoreChat(chgDM, "Alice", chgTime); err != nil {
		t.Fatal(err)
	}

	// Newest first, as WhatsApp sends history: the revoke and the edit come
	// before the messages they change.
	stub := historyWebMessageAt("h-stub", false, chgTime.Add(4*time.Minute), nil)
	stub.MessageStubType = waProto.WebMessageInfo_REVOKE.Enum()
	conv := &waProto.Conversation{
		ID: proto.String(chgDM),
		Messages: []*waProto.HistorySyncMsg{
			{Message: historyWebMessageAt("h-rev", false, chgTime.Add(5*time.Minute), revokePayload("h-1"))},
			{Message: stub},
			{Message: historyWebMessageAt("h-edit", false, chgTime.Add(3*time.Minute), editPayload("h-2", text("zweite, korrigiert")))},
			{Message: historyWebMessageAt("h-2", false, chgTime.Add(2*time.Minute), text("zweite"))},
			{Message: historyWebMessageAt("h-1", false, chgTime.Add(time.Minute), text("erste, gelöscht"))},
		},
	}
	// h-stub was cached live before the sync replaced it with a stub.
	if err := s.StoreMessage(t.Context(), store.Message{ID: "h-stub", ChatJID: chgDM, Sender: "447700000001", Content: "live, dann gelöscht", Timestamp: chgTime}, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}

	c.handleHistorySync(&events.HistorySync{Data: &waProto.HistorySync{Conversations: []*waProto.Conversation{conv}}})

	if r := readChg(t, s, chgDM, "h-1"); r.content != "" || !r.revoked {
		t.Errorf("h-1 (revoked in history): %+v", r)
	}
	if r := readChg(t, s, chgDM, "h-stub"); r.content != "" || !r.revoked {
		t.Errorf("h-stub (revoke stub): %+v", r)
	}
	if r := readChg(t, s, chgDM, "h-2"); r.content != "zweite, korrigiert" || !r.edited || r.revoked {
		t.Errorf("h-2 (edited in history): %+v", r)
	}
	var ts time.Time
	if err := s.DB().QueryRow("SELECT timestamp FROM messages WHERE id = 'h-2'").Scan(&ts); err != nil || !ts.Equal(chgTime.Add(2*time.Minute)) {
		t.Errorf("h-2 timestamp = %v (err %v), want the original's", ts, err)
	}
	for _, id := range []string{"h-rev", "h-edit"} {
		var n int
		_ = s.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE id = ?", id).Scan(&n)
		if n != 0 {
			t.Errorf("protocol message %s stored as a row", id)
		}
	}
}

// TestHandleHistorySync_CaptionEditKeepsMediaAndTime: a history caption edit
// of an image cached earlier changes only the text. The media key (needed by
// download_media) and the original's time stay.
func TestHandleHistorySync_CaptionEditKeepsMediaAndTime(t *testing.T) {
	c, s := newHistoryChangeClient(t)
	c.handleMessage(liveMessage(t, chgDM, chgDM, "img-h", false, chgTime,
		&waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Foto alt"), MediaKey: []byte{9}, DirectPath: proto.String("/v/t62/x")}}))

	edit := historyWebMessageAt("edit-h", false, chgTime.Add(time.Hour),
		editPayload("img-h", &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Foto neu")}}))
	c.handleHistorySync(historySync(chgDM, edit))

	if r := readChg(t, s, chgDM, "img-h"); r.content != "Foto neu" || !r.edited || !r.mediaKeyPresent {
		t.Errorf("img-h = %+v, want the new caption with the media key kept", r)
	}
	var ts time.Time
	var directPath string
	if err := s.DB().QueryRow("SELECT timestamp, direct_path FROM messages WHERE id = 'img-h'").Scan(&ts, &directPath); err != nil {
		t.Fatal(err)
	}
	if !ts.Equal(chgTime) || directPath != "/v/t62/x" {
		t.Errorf("timestamp = %v, direct_path = %q; want the original's", ts, directPath)
	}
}

func TestHistoryMessageFromKey_Edit(t *testing.T) {
	chat := types.NewJID("123456789", types.GroupServer)
	wm := historyWebMessage("k-edit", false, "", editPayload("k-orig", text("neu")))
	hm := historyMessageFromKey(chat, "491700000000", wm)
	if !hm.edited || hm.id != "k-orig" || extractTextContent(hm.message) != "neu" {
		t.Errorf("fallback edit = %+v", hm)
	}
}

// newHistoryChangeClient returns a client for history-sync tests with both
// test chats named, so GetChatName stays away from the network.
func newHistoryChangeClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithContacts(t, s, "491700000000", nil)
	for jid, name := range map[string]string{chgDM: "Alice", chgGroup: "Project Team"} {
		if err := s.StoreChat(jid, name, chgTime); err != nil {
			t.Fatal(err)
		}
	}
	return c, s
}

func historySync(chat string, msgs ...*waProto.WebMessageInfo) *events.HistorySync {
	conv := &waProto.Conversation{ID: proto.String(chat)}
	for _, m := range msgs {
		conv.Messages = append(conv.Messages, &waProto.HistorySyncMsg{Message: m})
	}
	return &events.HistorySync{Data: &waProto.HistorySync{Conversations: []*waProto.Conversation{conv}}}
}

// TestHandleHistorySync_EditsOldestFirst: a message edited twice ends up with
// the newest edit, whether history lists both edits before the original
// (newest first), without the original, or replays them after the newest
// edit was already applied live.
func TestHandleHistorySync_EditsOldestFirst(t *testing.T) {
	e2 := historyWebMessageAt("e-2", false, chgTime.Add(3*time.Minute), editPayload("m-1", text("dritte Fassung")))
	e1 := historyWebMessageAt("e-1", false, chgTime.Add(2*time.Minute), editPayload("m-1", text("zweite Fassung")))
	orig := historyWebMessageAt("m-1", false, chgTime.Add(time.Minute), text("erste Fassung"))

	t.Run("with original", func(t *testing.T) {
		c, s := newHistoryChangeClient(t)
		c.handleHistorySync(historySync(chgDM, e2, e1, orig))
		if r := readChg(t, s, chgDM, "m-1"); r.content != "dritte Fassung" || !r.edited {
			t.Errorf("m-1 = %+v, want the newest edit", r)
		}
	})
	t.Run("original not cached", func(t *testing.T) {
		c, s := newHistoryChangeClient(t)
		c.handleHistorySync(historySync(chgDM, e2, e1))
		if r := readChg(t, s, chgDM, "m-1"); r.content != "dritte Fassung" || !r.edited {
			t.Errorf("m-1 = %+v, want the newest edit", r)
		}
	})
	t.Run("replay after live edit", func(t *testing.T) {
		c, s := newHistoryChangeClient(t)
		c.handleMessage(liveMessage(t, chgDM, chgDM, "m-1", false, chgTime.Add(time.Minute), text("erste Fassung")))
		c.handleMessage(liveMessage(t, chgDM, chgDM, "e-2", false, chgTime.Add(3*time.Minute), editPayload("m-1", text("dritte Fassung"))))
		c.handleHistorySync(historySync(chgDM, e2, e1, orig))
		if r := readChg(t, s, chgDM, "m-1"); r.content != "dritte Fassung" || !r.edited {
			t.Errorf("m-1 = %+v, want the newest edit", r)
		}
	})
}

// TestHandleHistorySync_ForeignEditBeforeOriginal: in a group, member B's
// edit of A's message comes first in history (newest first). It must not end
// up as A's text marked [edited], neither when A's original follows in the
// same payload nor when it arrives later.
func TestHandleHistorySync_ForeignEditBeforeOriginal(t *testing.T) {
	const alice, bob = "447700000001@s.whatsapp.net", "447700000002@s.whatsapp.net"
	forged := historyWebMessageAt("b-edit", false, chgTime.Add(2*time.Minute), editPayload("g-1", text("Bob war hier")))
	forged.Participant = proto.String(bob)
	orig := historyWebMessageAt("g-1", false, chgTime.Add(time.Minute), text("Alice schreibt"))
	orig.Participant = proto.String(alice)

	readSender := func(t *testing.T, s *store.Store) string {
		t.Helper()
		var sender string
		if err := s.DB().QueryRow("SELECT sender FROM messages WHERE id = 'g-1' AND chat_jid = ?", chgGroup).Scan(&sender); err != nil {
			t.Fatalf("read sender: %v", err)
		}
		return sender
	}

	t.Run("same payload", func(t *testing.T) {
		c, s := newHistoryChangeClient(t)
		c.handleHistorySync(historySync(chgGroup, forged, orig))
		if r := readChg(t, s, chgGroup, "g-1"); r.content != "Alice schreibt" || r.edited {
			t.Errorf("g-1 = %+v, want Alice's text unedited", r)
		}
		if got := readSender(t, s); got != "447700000001" {
			t.Errorf("sender = %q, want Alice", got)
		}
	})
	t.Run("original arrives later", func(t *testing.T) {
		c, s := newHistoryChangeClient(t)
		c.handleHistorySync(historySync(chgGroup, forged))
		c.handleMessage(liveMessage(t, chgGroup, alice, "g-1", false, chgTime.Add(time.Minute), text("Alice schreibt")))
		if r := readChg(t, s, chgGroup, "g-1"); r.content != "Alice schreibt" || r.edited {
			t.Errorf("g-1 = %+v, want Alice's text unedited", r)
		}
		if got := readSender(t, s); got != "447700000001" {
			t.Errorf("sender = %q, want Alice", got)
		}
	})
	t.Run("author edit kept", func(t *testing.T) {
		c, s := newHistoryChangeClient(t)
		own := historyWebMessageAt("a-edit", false, chgTime.Add(2*time.Minute), editPayload("g-1", text("Alice korrigiert")))
		own.Participant = proto.String(alice)
		c.handleHistorySync(historySync(chgGroup, own, orig))
		if r := readChg(t, s, chgGroup, "g-1"); r.content != "Alice korrigiert" || !r.edited {
			t.Errorf("g-1 = %+v, want Alice's edit", r)
		}
	})
}

// TestHandleHistorySync_RevokeBeforeOriginalInLaterSync: a history sync can
// send a delete in one event and the deleted message in a later one. The
// delete waits as pending and is applied when the message is stored, so the
// deleted text does not come back.
func TestHandleHistorySync_RevokeBeforeOriginalInLaterSync(t *testing.T) {
	c, s := newHistoryChangeClient(t)

	c.handleHistorySync(historySync(chgDM,
		historyWebMessageAt("rev-early", false, chgTime.Add(5*time.Minute), revokePayload("orig-late"))))
	c.handleHistorySync(historySync(chgDM,
		historyWebMessageAt("orig-late", false, chgTime.Add(time.Minute), text("Kontonummer, bitte löschen"))))

	if r := readChg(t, s, chgDM, "orig-late"); r.content != "" || !r.revoked {
		t.Errorf("orig-late = %+v, want blanked and revoked", r)
	}
	var pending int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM pending_revokes").Scan(&pending); err != nil || pending != 0 {
		t.Errorf("pending revokes left = %d (err %v), want 0", pending, err)
	}
}

// TestHandleMessage_RevokeBeforeOriginal: the same for a live delete of a
// message that only arrives afterwards (a retried delivery, a sync).
func TestHandleMessage_RevokeBeforeOriginal(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleMessage(liveMessage(t, chgDM, chgDM, "rev-live", false, chgTime.Add(time.Minute), revokePayload("late-live")))
	c.handleMessage(liveMessage(t, chgDM, chgDM, "late-live", false, chgTime, text("doch nicht")))

	if r := readChg(t, s, chgDM, "late-live"); r.content != "" || !r.revoked {
		t.Errorf("late-live = %+v, want blanked and revoked", r)
	}
}

// TestOwnChange_UpdatesCacheForOwnSends: edit_message and delete_message
// update the cached row themselves, since whatsmeow does not echo our own
// sends. Only our own message is changed; a revoke of someone else's (as a
// group admin) leaves the cache alone.
func TestOwnChange_UpdatesCacheForOwnSends(t *testing.T) {
	const own = "491700000000"
	s := newTestStoreWithLIDMap(t, map[string]string{"99887766": "447700000001"})
	c := newClientWithContacts(t, s, own, nil)
	if err := s.StoreChat(chgDM, "Alice", chgTime); err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.Message{
		{ID: "own-1", ChatJID: chgDM, Sender: own, IsFromMe: true, Content: "Termin Dienstag", Timestamp: chgTime},
		{ID: "own-2", ChatJID: chgDM, Sender: own, IsFromMe: true, Content: "falscher Chat", Timestamp: chgTime},
		{ID: "theirs", ChatJID: chgDM, Sender: "447700000001", Content: "Danke", Timestamp: chgTime},
	} {
		if err := s.StoreMessage(t.Context(), m, nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}

	// The tool may be called with the chat's LID; the cache keys it by phone.
	lidChat := types.NewJID("99887766", types.HiddenUserServer)
	if res := c.applyEdit(t.Context(), c.ownChange(lidChat, "own-1"), "Termin Mittwoch"); res != store.ChangeApplied {
		t.Fatalf("own edit = %v, want applied", res)
	}
	if r := readChg(t, s, chgDM, "own-1"); r.content != "Termin Mittwoch" || !r.edited {
		t.Errorf("own-1 = %+v, want the new text with edited_at", r)
	}

	dm := types.NewJID("447700000001", types.DefaultUserServer)
	if res := c.applyRevoke(t.Context(), c.ownChange(dm, "own-2")); res != store.ChangeApplied {
		t.Fatalf("own revoke = %v, want applied", res)
	}
	if r := readChg(t, s, chgDM, "own-2"); r.content != "" || !r.revoked {
		t.Errorf("own-2 = %+v, want blanked and revoked", r)
	}

	if res := c.applyRevoke(t.Context(), c.ownChange(dm, "theirs")); res != store.ChangeSenderMismatch {
		t.Errorf("revoke of someone else's message = %v, want sender mismatch", res)
	}
	if r := readChg(t, s, chgDM, "theirs"); r.revoked || r.content != "Danke" {
		t.Errorf("theirs = %+v, want untouched", r)
	}
}
