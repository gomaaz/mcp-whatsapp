package client

import (
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestHealth_ZeroValue(t *testing.T) {
	c := &Client{}
	if h := c.Health(); h != (Health{}) {
		t.Fatalf("fresh Health = %+v, want zero value", h)
	}
}

func TestHealth_ConnectThenDisconnect(t *testing.T) {
	var tr healthTracker
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	tr.connected(t0)
	if got := tr.snapshot().ConnectedSince; !got.Equal(t0) {
		t.Fatalf("ConnectedSince = %v, want %v", got, t0)
	}

	tr.disconnected(t1)
	h := tr.snapshot()
	if !h.ConnectedSince.IsZero() {
		t.Errorf("ConnectedSince = %v after disconnect, want zero", h.ConnectedSince)
	}
	if !h.LastDisconnectAt.Equal(t1) {
		t.Errorf("LastDisconnectAt = %v, want %v", h.LastDisconnectAt, t1)
	}

	// Reconnecting starts a new session but keeps the disconnect history.
	t2 := t1.Add(time.Minute)
	tr.connected(t2)
	h = tr.snapshot()
	if !h.ConnectedSince.Equal(t2) || !h.LastDisconnectAt.Equal(t1) {
		t.Errorf("after reconnect got %+v, want ConnectedSince=%v LastDisconnectAt=%v", h, t2, t1)
	}
}

func TestHealth_LoggedOut(t *testing.T) {
	cases := []struct {
		name       string
		evt        *events.LoggedOut
		wantReason string
		wantSuffix bool
	}{
		{
			name:       "connect failure",
			evt:        &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureMainDeviceGone},
			wantReason: events.ConnectFailureMainDeviceGone.String(),
		},
		{
			name:       "device removed stream error",
			evt:        &events.LoggedOut{OnConnect: false, Reason: events.ConnectFailureLoggedOut},
			wantReason: events.ConnectFailureLoggedOut.String(),
			wantSuffix: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr healthTracker
			t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
			tr.connected(t0)
			at := t0.Add(20 * 24 * time.Hour)
			tr.loggedOut(at, tc.evt)

			h := tr.snapshot()
			if !h.LoggedOutAt.Equal(at) || !h.LastDisconnectAt.Equal(at) {
				t.Errorf("LoggedOutAt=%v LastDisconnectAt=%v, want both %v", h.LoggedOutAt, h.LastDisconnectAt, at)
			}
			if !h.ConnectedSince.IsZero() {
				t.Errorf("ConnectedSince = %v after logout, want zero", h.ConnectedSince)
			}
			if !strings.HasPrefix(h.LogoutReason, tc.wantReason) {
				t.Errorf("LogoutReason = %q, want prefix %q", h.LogoutReason, tc.wantReason)
			}
			if hasSuffix := strings.Contains(h.LogoutReason, "device removed"); hasSuffix != tc.wantSuffix {
				t.Errorf("LogoutReason = %q, device-removed note present = %v, want %v", h.LogoutReason, hasSuffix, tc.wantSuffix)
			}
		})
	}
}

func TestHealth_TemporaryBan(t *testing.T) {
	var tr healthTracker
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	tr.temporaryBan(at, &events.TemporaryBan{Code: events.TempBanBlockedByUsers, Expire: 2 * time.Hour})

	h := tr.snapshot()
	if want := at.Add(2 * time.Hour); !h.TemporaryBanUntil.Equal(want) {
		t.Errorf("TemporaryBanUntil = %v, want %v", h.TemporaryBanUntil, want)
	}
	if h.TemporaryBanReason != events.TempBanBlockedByUsers.String() {
		t.Errorf("TemporaryBanReason = %q, want %q", h.TemporaryBanReason, events.TempBanBlockedByUsers.String())
	}
	if !h.LastDisconnectAt.Equal(at) {
		t.Errorf("LastDisconnectAt = %v, want %v", h.LastDisconnectAt, at)
	}

	// No expiry from WhatsApp: the end time stays unknown (zero).
	tr.temporaryBan(at, &events.TemporaryBan{Code: events.TempBanSentToTooManyPeople})
	if h := tr.snapshot(); !h.TemporaryBanUntil.IsZero() {
		t.Errorf("TemporaryBanUntil = %v with no expiry, want zero", h.TemporaryBanUntil)
	}
}

// TestHandleEvent_RecordsConnectionEvents drives the real event dispatcher so
// the bookkeeping stays wired to the whatsmeow event types it expects.
func TestHandleEvent_RecordsConnectionEvents(t *testing.T) {
	c := newClientWithStore(t, nil)

	before := time.Now()
	c.handleEvent(&events.Connected{})
	if h := c.Health(); h.ConnectedSince.Before(before) {
		t.Fatalf("ConnectedSince = %v, want >= %v", h.ConnectedSince, before)
	}

	c.handleEvent(&events.Disconnected{})
	if h := c.Health(); !h.ConnectedSince.IsZero() || h.LastDisconnectAt.Before(before) {
		t.Fatalf("after Disconnected got %+v", h)
	}

	c.handleEvent(&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut})
	if h := c.Health(); h.LoggedOutAt.IsZero() || h.LogoutReason == "" {
		t.Fatalf("after LoggedOut got %+v", h)
	}

	c.handleEvent(&events.TemporaryBan{Code: events.TempBanBroadcastList, Expire: time.Minute})
	if h := c.Health(); h.TemporaryBanUntil.IsZero() || h.TemporaryBanReason == "" {
		t.Fatalf("after TemporaryBan got %+v", h)
	}
}

func TestHandleMessage_RecordsIncomingOnly(t *testing.T) {
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithStore(t, s)

	// Seed a chat name so GetChatName never needs the whatsmeow client.
	const chatJID = "447700000001@s.whatsapp.net"
	if err := s.StoreChat(chatJID, "Alice", time.Now()); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	jid := types.NewJID("447700000001", types.DefaultUserServer)
	newMsg := func(id string, fromMe bool) *events.Message {
		return &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: jid, Sender: jid, IsFromMe: fromMe},
				ID:            id,
				Timestamp:     time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC),
			},
			Message: &waProto.Message{Conversation: proto.String("hello")},
		}
	}

	c.handleMessage(newMsg("own-1", true))
	if h := c.Health(); !h.LastMessageReceivedAt.IsZero() {
		t.Fatalf("own message set LastMessageReceivedAt = %v, want zero", h.LastMessageReceivedAt)
	}

	before := time.Now()
	c.handleMessage(newMsg("in-1", false))
	if h := c.Health(); h.LastMessageReceivedAt.Before(before) {
		t.Fatalf("LastMessageReceivedAt = %v, want >= %v", h.LastMessageReceivedAt, before)
	}
}
