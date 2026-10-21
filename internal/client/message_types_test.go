package client

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/sealjay/mcp-whatsapp/internal/store"
)

const aliceVCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nN:Example;Alice;;;\r\nFN:Alice Example\r\n" +
	"item1.TEL;waid=491701234567:+49 170 1234567\r\nitem1.X-ABLabel:Mobil\r\nEND:VCARD\r\n"

func TestExtractTextContent_Placeholders(t *testing.T) {
	cases := []struct {
		name string
		msg  *waProto.Message
		want string
	}{
		{"location", &waProto.Message{LocationMessage: &waProto.LocationMessage{
			DegreesLatitude: proto.Float64(52.520008), DegreesLongitude: proto.Float64(13.404954),
			Name: proto.String("Büro"), Address: proto.String("Hauptstraße 1, Berlin"),
		}}, "[location] Büro, Hauptstraße 1, Berlin (52.520008,13.404954) https://maps.google.com/?q=52.520008,13.404954"},
		{"location without labels", &waProto.Message{LocationMessage: &waProto.LocationMessage{
			DegreesLatitude: proto.Float64(-33.5), DegreesLongitude: proto.Float64(151),
		}}, "[location] (-33.5,151) https://maps.google.com/?q=-33.5,151"},
		{"live location flag", &waProto.Message{LocationMessage: &waProto.LocationMessage{
			DegreesLatitude: proto.Float64(1.25), DegreesLongitude: proto.Float64(2.5), IsLive: proto.Bool(true),
		}}, "[live location] (1.25,2.5) https://maps.google.com/?q=1.25,2.5"},
		{"live location", &waProto.Message{LiveLocationMessage: &waProto.LiveLocationMessage{
			DegreesLatitude: proto.Float64(48.1), DegreesLongitude: proto.Float64(11.6), Caption: proto.String("bin unterwegs"),
		}}, "[live location] bin unterwegs (48.1,11.6) https://maps.google.com/?q=48.1,11.6"},
		{"contact card", &waProto.Message{ContactMessage: &waProto.ContactMessage{
			DisplayName: proto.String("Alice"), Vcard: proto.String(aliceVCard),
		}}, "[contact] Alice, TEL +49 170 1234567"},
		{"contact card name from vCard", &waProto.Message{ContactMessage: &waProto.ContactMessage{
			Vcard: proto.String(aliceVCard),
		}}, "[contact] Alice Example, TEL +49 170 1234567"},
		{"contacts array", &waProto.Message{ContactsArrayMessage: &waProto.ContactsArrayMessage{
			DisplayName: proto.String("2 contacts"),
			Contacts: []*waProto.ContactMessage{
				{DisplayName: proto.String("Alice")},
				{Vcard: proto.String("BEGIN:VCARD\nFN:Bob\nEND:VCARD")},
			},
		}}, "[contacts: 2] Alice, Bob"},
		{"video note caption", &waProto.Message{PtvMessage: &waProto.VideoMessage{Caption: proto.String("kurz erklärt")}}, "kurz erklärt"},
		{"sticker has no text", &waProto.Message{StickerMessage: &waProto.StickerMessage{}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractTextContent(tc.msg); got != tc.want {
				t.Errorf("extractTextContent =\n %q\nwant\n %q", got, tc.want)
			}
		})
	}
}

func TestExtractMediaInfo_StickerAndVideoNote(t *testing.T) {
	sha := []byte{0xab, 0xcd}
	mediaType, filename, url, directPath, key, _, _, length := extractMediaInfo(&waProto.Message{StickerMessage: &waProto.StickerMessage{
		URL: proto.String("https://mmg.whatsapp.net/s"), DirectPath: proto.String("/s?ccb=1"),
		MediaKey: []byte{1}, FileSHA256: sha, FileEncSHA256: []byte{2}, FileLength: proto.Uint64(42),
	}})
	if mediaType != "sticker" || filename != "sticker_abcd.webp" || url == "" || directPath != "/s?ccb=1" || len(key) == 0 || length != 42 {
		t.Errorf("sticker media = %q %q %q %q key=%v len=%d", mediaType, filename, url, directPath, key, length)
	}

	mediaType, filename, _, directPath, key, _, _, _ = extractMediaInfo(&waProto.Message{PtvMessage: &waProto.VideoMessage{
		DirectPath: proto.String("/v"), MediaKey: []byte{3}, FileSHA256: sha,
	}})
	if mediaType != "video" || filename != "video_abcd.mp4" || directPath != "/v" || len(key) == 0 {
		t.Errorf("video note media = %q %q %q key=%v", mediaType, filename, directPath, key)
	}
}

func TestParseVCard(t *testing.T) {
	cases := []struct {
		name, card, wantName string
		wantTels             []string
	}{
		{"grouped TEL", aliceVCard, "Alice Example", []string{"+49 170 1234567"}},
		{"folded FN and escapes", "BEGIN:VCARD\r\nFN:Müller\\, Hans\r\n  Peter\r\nTEL;TYPE=CELL:+49 30 1\r\nTEL:+49 30 1\r\nTEL:+44 20 2\r\nEND:VCARD",
			"Müller, Hans Peter", []string{"+49 30 1", "+44 20 2"}},
		{"N fallback, waid only, uri value", "BEGIN:VCARD\nN:Doe;Jane;;;\nTEL;waid=4917099:\nTEL;VALUE=uri:tel:+1-555-0100\nEND:VCARD",
			"Jane Doe", []string{"+4917099", "+1-555-0100"}},
		{"empty", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, tels := parseVCard(tc.card)
			if name != tc.wantName || !reflect.DeepEqual(tels, tc.wantTels) {
				t.Errorf("parseVCard = %q %q, want %q %q", name, tels, tc.wantName, tc.wantTels)
			}
		})
	}
}

// TestParseVCard_RoundTrip reads back what buildVCard writes for send_contact.
func TestParseVCard_RoundTrip(t *testing.T) {
	name, tels := parseVCard(buildVCard("Erika; Mustermann, GmbH", "+49 170 555"))
	if name != "Erika; Mustermann, GmbH" || !reflect.DeepEqual(tels, []string{"+49170555"}) {
		t.Errorf("round trip = %q %q", name, tels)
	}
}

type storedRow struct {
	content, mediaType, filename string
	fromMe                       bool
}

func readStored(t *testing.T, s *store.Store, chat, id string) (storedRow, bool) {
	t.Helper()
	var r storedRow
	var mediaType, filename sql.NullString
	err := s.DB().QueryRow("SELECT content, media_type, filename, is_from_me FROM messages WHERE id = ? AND chat_jid = ?", id, chat).
		Scan(&r.content, &mediaType, &filename, &r.fromMe)
	if err == sql.ErrNoRows {
		return r, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	r.mediaType, r.filename = mediaType.String, filename.String
	return r, true
}

func TestHandleMessage_StoresPlaceholderTypes(t *testing.T) {
	c, s := newChangeClient(t)
	msgs := map[string]*waProto.Message{
		"loc":     {LocationMessage: &waProto.LocationMessage{DegreesLatitude: proto.Float64(52.5), DegreesLongitude: proto.Float64(13.4), Name: proto.String("Baustelle")}},
		"live":    {LiveLocationMessage: &waProto.LiveLocationMessage{DegreesLatitude: proto.Float64(52.5), DegreesLongitude: proto.Float64(13.4)}},
		"contact": {ContactMessage: &waProto.ContactMessage{DisplayName: proto.String("Alice"), Vcard: proto.String(aliceVCard)}},
		"many":    {ContactsArrayMessage: &waProto.ContactsArrayMessage{Contacts: []*waProto.ContactMessage{{DisplayName: proto.String("A")}, {DisplayName: proto.String("B")}}}},
		"sticker": {StickerMessage: &waProto.StickerMessage{FileSHA256: []byte{0x01}, MediaKey: []byte{1}}},
		"ptv":     {PtvMessage: &waProto.VideoMessage{FileSHA256: []byte{0x02}, MediaKey: []byte{2}}},
	}
	i := 0
	for id, m := range msgs {
		i++
		c.handleMessage(liveMessage(t, chgDM, chgDM, id, false, chgTime.Add(time.Duration(i)*time.Minute), m))
	}
	want := map[string]storedRow{
		"loc":     {content: "[location] Baustelle (52.5,13.4) https://maps.google.com/?q=52.5,13.4"},
		"live":    {content: "[live location] (52.5,13.4) https://maps.google.com/?q=52.5,13.4"},
		"contact": {content: "[contact] Alice, TEL +49 170 1234567"},
		"many":    {content: "[contacts: 2] A, B"},
		"sticker": {mediaType: "sticker", filename: "sticker_01.webp"},
		"ptv":     {mediaType: "video", filename: "video_02.mp4"},
	}
	for id, w := range want {
		got, ok := readStored(t, s, chgDM, id)
		if !ok {
			t.Errorf("%s was dropped", id)
			continue
		}
		if got != w {
			t.Errorf("%s stored as %+v, want %+v", id, got, w)
		}
	}
}

func TestDownload_StickerUsesImageKeys(t *testing.T) {
	t.Chdir(t.TempDir())
	s, err := store.Open("./store")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := newClientWithStore(t, s)
	if err := s.StoreChat(chgDM, "Alice", chgTime); err != nil {
		t.Fatal(err)
	}
	payload := []byte("RIFF....WEBP")
	nm := c.normalizeIncomingMessage(chgDM, chgDM, "447700000001", false, false, "", "st-1", chgTime,
		&waProto.Message{StickerMessage: &waProto.StickerMessage{
			URL: proto.String("https://mmg.whatsapp.net/x"), DirectPath: proto.String("/x"),
			MediaKey: []byte{1}, FileSHA256: sha256Of(payload), FileEncSHA256: []byte{2}, FileLength: proto.Uint64(uint64(len(payload))),
		}})
	if err := s.StoreMessage(context.Background(), nm.msg, nm.mediaKey, nm.fileSHA256, nm.fileEncSHA256, nm.fileLength); err != nil {
		t.Fatal(err)
	}
	var gotType whatsmeow.MediaType
	c.downloadMedia = func(_ context.Context, d whatsmeow.DownloadableMessage) ([]byte, error) {
		gotType = d.(*MediaDownloader).MediaType
		return payload, nil
	}
	res := c.Download(context.Background(), "st-1", chgDM, "")
	if !res.Success || res.MediaType != "sticker" || !strings.HasSuffix(res.Path, ".webp") {
		t.Fatalf("download = %+v", res)
	}
	if gotType != whatsmeow.MediaImage {
		t.Errorf("media type = %q, want image keys", gotType)
	}
	if _, err := os.Stat(filepath.Clean(res.Path)); err != nil {
		t.Errorf("sticker not saved: %v", err)
	}
}

func undecryptable(t *testing.T, chat, sender, id string, ts time.Time, mode events.DecryptFailMode) *events.UndecryptableMessage {
	t.Helper()
	m := liveMessage(t, chat, sender, id, false, ts, nil)
	return &events.UndecryptableMessage{Info: m.Info, DecryptFailMode: mode}
}

func TestHandleUndecryptable_PlaceholderReplacedByRetry(t *testing.T) {
	const chat = "447700000005@s.whatsapp.net"
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithContacts(t, s, "491700000000", map[types.JID]types.ContactInfo{
		types.NewJID("447700000005", types.DefaultUserServer): {Found: true, FullName: "Bob Kunde"},
	})
	// No chat row yet: the placeholder has to create it first (foreign key).
	c.handleEvent(undecryptable(t, chat, chat, "u-1", chgTime, events.DecryptFailShow))

	got, ok := readStored(t, s, chat, "u-1")
	if !ok || got.content != store.UndecryptablePlaceholder || got.mediaType != "" || got.fromMe {
		t.Fatalf("placeholder = %+v (stored %v)", got, ok)
	}
	if name := s.FindChatName(chat); name != "Bob Kunde" {
		t.Errorf("chat name = %q", name)
	}

	// The retry decrypts: the real message replaces the placeholder.
	c.handleMessage(liveMessage(t, chat, chat, "u-1", false, chgTime, text("Angebot bitte bis Freitag")))
	if got, _ := readStored(t, s, chat, "u-1"); got.content != "Angebot bitte bis Freitag" {
		t.Errorf("after retry content = %q", got.content)
	}

	// A late repeat of the failure never overwrites the decrypted message.
	c.handleEvent(undecryptable(t, chat, chat, "u-1", chgTime, events.DecryptFailShow))
	if got, _ := readStored(t, s, chat, "u-1"); got.content != "Angebot bitte bis Freitag" {
		t.Errorf("repeated failure replaced the message: %q", got.content)
	}
}

func TestHandleUndecryptable_GroupHiddenAndStatus(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleEvent(undecryptable(t, chgGroup, "447700000002@s.whatsapp.net", "u-g", chgTime, events.DecryptFailShow))
	if got, ok := readStored(t, s, chgGroup, "u-g"); !ok || got.content != store.UndecryptablePlaceholder {
		t.Errorf("group placeholder = %+v (stored %v)", got, ok)
	}

	c.handleEvent(undecryptable(t, chgGroup, "447700000002@s.whatsapp.net", "u-hide", chgTime, events.DecryptFailHide))
	if _, ok := readStored(t, s, chgGroup, "u-hide"); ok {
		t.Error("a message WhatsApp marks hidden got a placeholder")
	}

	status := undecryptable(t, "status@broadcast", "447700000002@s.whatsapp.net", "u-status", chgTime, events.DecryptFailShow)
	c.handleEvent(status)
	if _, ok := readStored(t, s, "status@broadcast", "u-status"); ok {
		t.Error("a status update got a placeholder")
	}
}

func TestHandleMessage_DecryptedReactionDropsPlaceholder(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleEvent(undecryptable(t, chgDM, chgDM, "u-r", chgTime, events.DecryptFailShow))
	c.handleMessage(liveMessage(t, chgDM, chgDM, "u-r", false, chgTime, reactionPayload("orig", "👍", 0)))
	if _, ok := readStored(t, s, chgDM, "u-r"); ok {
		t.Error("placeholder left behind after the retry decrypted to a reaction")
	}
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func reactionPayload(target, emoji string, ms int64) *waProto.Message {
	rm := &waProto.ReactionMessage{Key: &waProto.MessageKey{ID: proto.String(target)}, Text: proto.String(emoji)}
	if ms > 0 {
		rm.SenderTimestampMS = proto.Int64(ms)
	}
	return &waProto.Message{ReactionMessage: rm}
}

type reactionRow struct {
	sender, emoji string
	fromMe        bool
}

func readReactions(t *testing.T, s *store.Store, chat, id string) []reactionRow {
	t.Helper()
	rows, err := s.DB().Query("SELECT sender, emoji, is_from_me FROM reactions WHERE chat_jid = ? AND message_id = ? ORDER BY sender", chat, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []reactionRow
	for rows.Next() {
		var r reactionRow
		if err := rows.Scan(&r.sender, &r.emoji, &r.fromMe); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestHandleMessage_LiveReactions(t *testing.T) {
	c, s := newChangeClient(t)
	c.handleMessage(liveMessage(t, chgGroup, "447700000002@s.whatsapp.net", "g-1", false, chgTime, text("Wer kommt mit?")))
	var before time.Time
	_ = s.DB().QueryRow("SELECT last_message_time FROM chats WHERE jid = ?", chgGroup).Scan(&before)

	// LID sender resolves to the phone number, like message senders.
	c.handleMessage(liveMessage(t, chgGroup, "99887766@lid", "r-1", false, chgTime.Add(time.Hour), reactionPayload("g-1", "👍", 0)))
	c.handleMessage(liveMessage(t, chgGroup, "447700000003@s.whatsapp.net", "r-2", false, chgTime.Add(time.Hour), reactionPayload("g-1", "❤️", 0)))
	// Our own reaction from another device.
	c.handleMessage(liveMessage(t, chgGroup, "491700000000@s.whatsapp.net", "r-3", true, chgTime.Add(time.Hour), reactionPayload("g-1", "😂", 0)))

	want := []reactionRow{
		{sender: "447700000002", emoji: "👍"},
		{sender: "447700000003", emoji: "❤️"},
		{sender: "491700000000", emoji: "😂", fromMe: true},
	}
	if got := readReactions(t, s, chgGroup, "g-1"); !reflect.DeepEqual(got, want) {
		t.Errorf("reactions = %+v, want %+v", got, want)
	}
	for _, id := range []string{"r-1", "r-2", "r-3"} {
		if _, ok := readStored(t, s, chgGroup, id); ok {
			t.Errorf("reaction %s stored as a message row", id)
		}
	}
	var after time.Time
	_ = s.DB().QueryRow("SELECT last_message_time FROM chats WHERE jid = ?", chgGroup).Scan(&after)
	if !after.Equal(before) {
		t.Errorf("a reaction moved the chat's last activity from %v to %v", before, after)
	}

	// Removal (empty emoji), then an older replay that must not undo it.
	c.handleMessage(liveMessage(t, chgGroup, "447700000003@s.whatsapp.net", "r-4", false, chgTime.Add(2*time.Hour), reactionPayload("g-1", "", 0)))
	c.handleMessage(liveMessage(t, chgGroup, "447700000003@s.whatsapp.net", "r-2", false, chgTime.Add(time.Hour), reactionPayload("g-1", "❤️", 0)))
	ctx, err := s.GetMessageContext(context.Background(), "g-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var emojis []string
	for _, r := range ctx.Message.Reactions {
		emojis = append(emojis, r.Emoji)
	}
	if !reflect.DeepEqual(emojis, []string{"👍", "😂"}) {
		t.Errorf("reactions after removal = %q", emojis)
	}
}

func TestHandleHistorySync_Reactions(t *testing.T) {
	c, s := newHistoryChangeClient(t)
	ms := chgTime.Add(time.Hour).UnixMilli()

	dm := historyWebMessageAt("hd-1", false, chgTime, text("Rechnung anbei"))
	dm.Reactions = []*waProto.Reaction{
		{Key: &waProto.MessageKey{FromMe: proto.Bool(true)}, Text: proto.String("👍"), SenderTimestampMS: proto.Int64(ms)},
		{Key: &waProto.MessageKey{FromMe: proto.Bool(false)}, Text: proto.String("🙏"), SenderTimestampMS: proto.Int64(ms)},
	}
	// A reaction message in the history list itself is not stored as a row.
	rx := historyWebMessageAt("hd-rx", false, chgTime.Add(2*time.Hour), reactionPayload("hd-1", "🔥", ms+1000))
	c.handleHistorySync(historySync(chgDM, rx, dm))

	got := readReactions(t, s, chgDM, "hd-1")
	want := []reactionRow{
		{sender: "447700000001", emoji: "🔥"},
		{sender: "491700000000", emoji: "👍", fromMe: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("history reactions = %+v, want %+v", got, want)
	}
	if _, ok := readStored(t, s, chgDM, "hd-rx"); ok {
		t.Error("history reaction stored as a message row")
	}

	grp := historyWebMessage("hg-1", false, "447700000002@s.whatsapp.net", text("Treffen um 8"))
	grp.MessageTimestamp = proto.Uint64(uint64(chgTime.Unix()))
	grp.Reactions = []*waProto.Reaction{{Key: &waProto.MessageKey{Participant: proto.String("447700000004@s.whatsapp.net")}, Text: proto.String("👌")}}
	c.handleHistorySync(historySync(chgGroup, grp))
	if got := readReactions(t, s, chgGroup, "hg-1"); !reflect.DeepEqual(got, []reactionRow{{sender: "447700000004", emoji: "👌"}}) {
		t.Errorf("group history reactions = %+v", got)
	}
}

func TestReactionUpdate_PrefersSenderTimestamp(t *testing.T) {
	nm := normalizedMessage{msg: store.Message{Sender: "4917", IsFromMe: true}}
	ms := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC).UnixMilli()
	u := reactionUpdate("c@s.whatsapp.net", nm, reactionPayload("t", "👍", ms).GetReactionMessage(), chgTime, "own")
	if u.Sender != "own" || !u.IsFromMe || u.MessageID != "t" || !u.Timestamp.Equal(time.UnixMilli(ms)) {
		t.Errorf("reactionUpdate = %+v", u)
	}
	far := time.Now().Add(48 * time.Hour).UnixMilli()
	if u := reactionUpdate("c", nm, reactionPayload("t", "👍", far).GetReactionMessage(), chgTime, ""); u.Timestamp.After(time.Now()) || u.Sender != "4917" {
		t.Errorf("future timestamp not clamped or sender lost: %+v", u)
	}
}
