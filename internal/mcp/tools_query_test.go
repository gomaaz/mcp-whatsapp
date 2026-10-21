package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// fakePairCache is a minimal pairingCache for exercising pairing_status
// without standing up the daemon's real PairCache.
type fakePairCache struct {
	paired bool
	qr     string
}

func (f fakePairCache) Paired() bool { return f.paired }
func (f fakePairCache) QR() string   { return f.qr }

// callPairingStatus invokes the pairing_status handler and decodes its JSON
// "setup_state" envelope into a map.
func callPairingStatus(t *testing.T, s *Server) map[string]any {
	t.Helper()
	tool, ok := s.MCP().ListTools()["pairing_status"]
	if !ok || tool == nil || tool.Handler == nil {
		t.Fatal("pairing_status not registered")
	}
	res, err := tool.Handler(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("pairing_status handler: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("pairing_status returned no content")
	}
	text, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("pairing_status content not text: %T", res.Content[0])
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(text.Text), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", text.Text, err)
	}
	if env["type"] != "setup_state" {
		t.Errorf("type = %v, want setup_state", env["type"])
	}
	return env
}

// A nil pairing cache (smoke/tests) yields the error envelope without touching
// the WhatsApp client — so passing a nil client here must not panic.
func TestPairingStatus_NilCache(t *testing.T) {
	env := callPairingStatus(t, NewServer(nil, nil))
	if env["state"] != "error" {
		t.Errorf("state = %v, want error", env["state"])
	}
	if env["detail"] != "pairing cache unavailable" {
		t.Errorf("detail = %v, want pairing cache unavailable", env["detail"])
	}
}

// An unpaired cache with a pending QR yields awaiting_qr carrying the payload.
func TestPairingStatus_AwaitingQRWithPayload(t *testing.T) {
	env := callPairingStatus(t, NewServer(nil, fakePairCache{qr: "2@QRPAYLOAD"}))
	if env["state"] != "awaiting_qr" {
		t.Fatalf("state = %v, want awaiting_qr", env["state"])
	}
	if env["qr_payload"] != "2@QRPAYLOAD" {
		t.Errorf("qr_payload = %v, want 2@QRPAYLOAD", env["qr_payload"])
	}
	if env["detail"] != "Scan with WhatsApp → Linked devices" {
		t.Errorf("detail = %v, want the scan instruction", env["detail"])
	}
}

// An unpaired cache with no QR yet yields the generating-code envelope and
// omits qr_payload.
func TestPairingStatus_AwaitingQRGenerating(t *testing.T) {
	env := callPairingStatus(t, NewServer(nil, fakePairCache{}))
	if env["state"] != "awaiting_qr" {
		t.Fatalf("state = %v, want awaiting_qr", env["state"])
	}
	if _, ok := env["qr_payload"]; ok {
		t.Errorf("qr_payload should be absent, got %v", env["qr_payload"])
	}
	if env["detail"] != "Generating pairing code…" {
		t.Errorf("detail = %v, want generating message", env["detail"])
	}
}

// list_messages advertises the direction, media-type and group filters.
func TestListMessages_SchemaHasFilterArgs(t *testing.T) {
	tool, ok := NewServer(nil, nil).MCP().ListTools()["list_messages"]
	if !ok || tool == nil {
		t.Fatal("list_messages not registered")
	}
	props := tool.Tool.InputSchema.Properties
	for _, name := range []string{"from_me", "media_type", "exclude_groups"} {
		if _, ok := props[name]; !ok {
			t.Errorf("list_messages schema missing %q", name)
		}
	}
	for _, name := range tool.Tool.InputSchema.Required {
		if name == "from_me" || name == "media_type" || name == "exclude_groups" {
			t.Errorf("%q must stay optional", name)
		}
	}
}

// An unknown media_type is rejected before the store is touched, so a nil
// client is safe here.
func TestListMessages_RejectsUnknownMediaType(t *testing.T) {
	tool, ok := NewServer(nil, nil).MCP().ListTools()["list_messages"]
	if !ok || tool == nil || tool.Handler == nil {
		t.Fatal("list_messages not registered")
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_messages"
	req.Params.Arguments = map[string]any{"media_type": "sticker"}
	res, err := tool.Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("list_messages handler: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected tool error for media_type=sticker, got %+v", res)
	}
	text, ok := mcp.AsTextContent(res.Content[0])
	if !ok || !strings.Contains(text.Text, "invalid 'media_type'") {
		t.Fatalf("unexpected error content: %+v", res.Content)
	}
}

func TestAddHealthFields_OmitsZeroValues(t *testing.T) {
	status := map[string]any{"connected": false}
	addHealthFields(status, client.Health{})
	if len(status) != 1 {
		t.Fatalf("zero Health added fields: %v", status)
	}
}

func TestAddHealthFields_FormatsRFC3339UTC(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*60*60)
	h := client.Health{
		ConnectedSince:        time.Date(2026, 10, 21, 10, 0, 0, 0, berlin),
		LastDisconnectAt:      time.Date(2026, 10, 21, 9, 59, 0, 0, time.UTC),
		LoggedOutAt:           time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC),
		LogoutReason:          "401: logged out from another device",
		TemporaryBanUntil:     time.Date(2026, 10, 2, 7, 0, 0, 0, time.UTC),
		TemporaryBanReason:    "102: too many people blocked you",
		LastMessageReceivedAt: time.Date(2026, 10, 21, 10, 5, 30, 0, time.UTC),
	}
	status := map[string]any{}
	addHealthFields(status, h)

	want := map[string]string{
		"connected_since":          "2026-10-21T08:00:00Z",
		"last_disconnect_at":       "2026-10-21T09:59:00Z",
		"logged_out_at":            "2026-10-01T07:00:00Z",
		"logout_reason":            "401: logged out from another device",
		"temporary_ban_until":      "2026-10-02T07:00:00Z",
		"temporary_ban_reason":     "102: too many people blocked you",
		"last_message_received_at": "2026-10-21T10:05:30Z",
	}
	if len(status) != len(want) {
		t.Errorf("got %d fields, want %d: %v", len(status), len(want), status)
	}
	for k, v := range want {
		if status[k] != v {
			t.Errorf("%s = %v, want %q", k, status[k], v)
		}
	}
}
