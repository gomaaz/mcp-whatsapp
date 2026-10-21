package client

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/sealjay/mcp-whatsapp/internal/security"
)

// captureLogger returns a stderrLogger writing every level into buf.
func captureLogger(buf *bytes.Buffer) *stderrLogger {
	return &stderrLogger{out: buf, mod: "test", min: logLevelToInt["DEBUG"]}
}

func TestStderrLogger_TimestampIsUTC(t *testing.T) {
	var buf bytes.Buffer
	captureLogger(&buf).Infof("hello")
	line := buf.String()
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z \[test INFO\] hello\n$`).MatchString(line) {
		t.Fatalf("log line %q does not start with a UTC timestamp ending in Z", line)
	}
	stamp, _, _ := strings.Cut(line, " ")
	got, err := time.Parse(logTimestampLayout, stamp)
	if err != nil {
		t.Fatalf("parse %q: %v", stamp, err)
	}
	if d := time.Since(got); d < -time.Minute || d > time.Minute {
		t.Errorf("timestamp %s is %s away from now: not UTC?", stamp, d)
	}
}

func TestLogTime_UTC(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*60*60)
	if got := logTime(time.Date(2026, 10, 21, 11, 12, 33, 0, berlin)); got != "2026-10-21T09:12:33Z" {
		t.Errorf("logTime = %q, want 2026-10-21T09:12:33Z", got)
	}
}

func TestRedactingLogger(t *testing.T) {
	var buf bytes.Buffer
	l := newRedactingLogger(captureLogger(&buf), &security.Redactor{}, "WARN")
	l.Infof("Updated LID to %s", "123456789012345@lid")
	l.Sub("Recv").Warnf("Error decrypting message from %s: %v", "491751234567:3@s.whatsapp.net", "bad mac")
	out := buf.String()
	if strings.Contains(out, "Updated LID") {
		t.Errorf("INFO line passed a WARN filter: %q", out)
	}
	if !strings.Contains(out, "[test/Recv WARN] Error decrypting message from …4567: bad mac") {
		t.Errorf("WARN line not scrubbed or not written: %q", out)
	}

	buf.Reset()
	l = newRedactingLogger(captureLogger(&buf), &security.Redactor{}, "")
	l.Debugf("percent %s stays", "100%")
	if !strings.Contains(buf.String(), "percent 100% stays") {
		t.Errorf("formatted line altered: %q", buf.String())
	}

	inner := captureLogger(&buf)
	if got := newRedactingLogger(inner, &security.Redactor{Debug: true}, "WARN"); got != inner {
		t.Error("debug mode must hand back the logger unchanged")
	}
}

// TestMessageFlow_LogsNoPersonalData runs a representative set of events
// (incoming text, document, edit, delete, group message from a LID,
// reaction, undecryptable message, own reply, history sync) through the
// handlers with a capturing logger at DEBUG and checks that, without -debug,
// no phone number, full JID, full message ID, name or message text reaches
// the log. The scrubbing wrapper of client.New is left out on purpose: the
// call sites themselves must redact.
func TestMessageFlow_LogsNoPersonalData(t *testing.T) {
	const (
		own       = "491700000000"
		customer  = "491751234567@s.whatsapp.net"
		group     = "120363012345678901@g.us"
		lidSender = "123456789012345@lid"
		lidPhone  = "491769876543"
		histGroup = "120363098765432109@g.us"
	)
	secrets := []string{
		// phone numbers and JIDs
		"491751234567", lidPhone, "123456789012345", "120363012345678901", "120363098765432109",
		"@s.whatsapp.net", "@lid", "@g.us",
		// message IDs
		"3EB0AA11BB22CC33DD44", "3EB0AA11BB22CC33DD55", "3EB0AA11BB22CC33DD66", "3EB0AA11BB22CC33DD77",
		"3EB0AA11BB22CC33DD88", "3EB0AA11BB22CC33DD99", "3EB0FF00EE11DD22CC33", "3EB0HISTORY000000001",
		// names
		"Karin", "Müller", "Mueller", "Schmidt", "Weber", "Rechnung",
		// message text
		"morgen", "übermorgen", "Hier die", "geliefert", "passt", "Fliesen",
	}

	run := func(t *testing.T, r *security.Redactor) string {
		t.Helper()
		var buf bytes.Buffer
		s := newTestStoreWithLIDMap(t, map[string]string{"123456789012345": lidPhone})
		custJID := types.NewJID("491751234567", types.DefaultUserServer)
		c := newClientWithContacts(t, s, own, map[types.JID]types.ContactInfo{
			custJID: {Found: true, FullName: "Karin Müller"},
		})
		c.log = captureLogger(&buf)
		c.redactor = r
		// A stored group name keeps GetChatName away from the network.
		if err := s.StoreChat(group, "Baustelle Schmidt", chgTime); err != nil {
			t.Fatal(err)
		}

		at := func(m int) time.Time { return chgTime.Add(time.Duration(m) * time.Minute) }
		c.handleEvent(liveMessage(t, customer, customer, "3EB0AA11BB22CC33DD44", false, at(1),
			text("Können Sie morgen um 9 kommen? Tel 0171 2345678")))
		c.handleEvent(liveMessage(t, customer, customer, "3EB0AA11BB22CC33DD55", false, at(2),
			&waProto.Message{DocumentMessage: &waProto.DocumentMessage{
				FileName: proto.String("Rechnung_Mueller.pdf"), Caption: proto.String("Hier die Rechnung"),
				MediaKey: []byte{1}, FileSHA256: []byte{2},
			}}))
		c.handleEvent(liveMessage(t, customer, customer, "3EB0AA11BB22CC33DD66", false, at(3),
			editPayload("3EB0AA11BB22CC33DD44", text("Können Sie übermorgen kommen?"))))
		c.handleEvent(liveMessage(t, customer, customer, "3EB0AA11BB22CC33DD77", false, at(4),
			revokePayload("3EB0AA11BB22CC33DD55")))
		c.handleEvent(liveMessage(t, group, lidSender, "3EB0AA11BB22CC33DD88", false, at(5),
			text("Material ist geliefert")))
		c.handleEvent(liveMessage(t, group, customer, "3EB0AA11BB22CC33DD99", false, at(6),
			reactionPayload("3EB0AA11BB22CC33DD88", "👍", 0)))
		c.handleEvent(undecryptable(t, customer, customer, "3EB0AA11BB22CC33DD33", at(7), events.DecryptFailShow))
		c.persistSent(context.Background(), custJID, "3EB0FF00EE11DD22CC33", "Ja, passt. Gruß", "", text("Ja, passt. Gruß"))

		hist := historyWebMessageAt("3EB0HISTORY000000001", false, at(8), text("Fliesen kommen Freitag"))
		hist.Participant = proto.String(customer)
		c.handleEvent(&events.HistorySync{Data: &waProto.HistorySync{Conversations: []*waProto.Conversation{{
			ID:          proto.String(histGroup),
			DisplayName: proto.String("Familie Weber"),
			Messages:    []*waProto.HistorySyncMsg{{Message: hist}},
		}}}})
		return buf.String()
	}

	out := run(t, &security.Redactor{})
	if !strings.Contains(out, "[name: 12 chars]") || !strings.Contains(out, "B: text]") {
		t.Fatalf("flow logged too little to be meaningful:\n%s", out)
	}
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
	if m := regexp.MustCompile(`\d{7,}`).FindString(out); m != "" {
		t.Errorf("log contains the digit run %q", m)
	}
	if t.Failed() {
		t.Logf("log output:\n%s", out)
	}

	// With -debug the same flow does show the content, so the checks above
	// are not passing on an empty or unrelated log.
	if dbg := run(t, &security.Redactor{Debug: true}); !strings.Contains(dbg, "Karin Müller") || !strings.Contains(dbg, "3EB0AA11BB22CC33DD44") {
		t.Errorf("debug log lacks the name or message ID:\n%s", dbg)
	}
}
