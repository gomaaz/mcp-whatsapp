package client

import (
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// Health is a snapshot of connection bookkeeping collected from whatsmeow
// events since the process started. It is purely local: nothing here is
// fetched from WhatsApp. Zero times mean "not seen since startup".
type Health struct {
	ConnectedSince        time.Time // start of the current session; zero while disconnected
	LastDisconnectAt      time.Time // most recent drop, logout or ban
	LoggedOutAt           time.Time // most recent events.LoggedOut
	LogoutReason          string    // reason attached to that logout
	TemporaryBanUntil     time.Time // expiry of the most recent temporary ban, when WhatsApp sent one
	TemporaryBanReason    string    // reason attached to that ban
	LastMessageReceivedAt time.Time // local time the last incoming (not own) message arrived
}

// healthTracker guards Health with a mutex. Event handlers run on whatsmeow's
// goroutines while get_status reads concurrently. The zero value is ready to
// use, so Client literals in tests need no setup.
type healthTracker struct {
	mu sync.Mutex
	h  Health
}

func (t *healthTracker) connected(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.h.ConnectedSince = now
}

func (t *healthTracker) disconnected(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.h.ConnectedSince = time.Time{}
	t.h.LastDisconnectAt = now
}

// loggedOut records a logout. whatsmeow always fills Reason; OnConnect=false
// means the phone removed this linked device while it was connected.
func (t *healthTracker) loggedOut(now time.Time, evt *events.LoggedOut) {
	reason := evt.Reason.String()
	if !evt.OnConnect {
		reason += " (device removed while connected)"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.h.ConnectedSince = time.Time{}
	t.h.LastDisconnectAt = now
	t.h.LoggedOutAt = now
	t.h.LogoutReason = reason
}

// temporaryBan records a temporary ban. An Expire of zero means WhatsApp did
// not say when it ends, so TemporaryBanUntil stays zero.
func (t *healthTracker) temporaryBan(now time.Time, evt *events.TemporaryBan) {
	var until time.Time
	if evt.Expire > 0 {
		until = now.Add(evt.Expire)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.h.ConnectedSince = time.Time{}
	t.h.LastDisconnectAt = now
	t.h.TemporaryBanUntil = until
	t.h.TemporaryBanReason = evt.Code.String()
}

func (t *healthTracker) messageReceived(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.h.LastMessageReceivedAt = now
}

func (t *healthTracker) snapshot() Health {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.h
}

// Health returns a copy of the connection bookkeeping for get_status.
func (c *Client) Health() Health {
	return c.health.snapshot()
}
