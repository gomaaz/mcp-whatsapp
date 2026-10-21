package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Central European summer and winter time, as a fixed zone so the tests do
// not depend on tzdata being installed (it is not on a bare Windows host).
var (
	cest = time.FixedZone("CEST", 2*60*60)
	cet  = time.FixedZone("CET", 1*60*60)
)

func TestDBTime_UTCAndSortsAsText(t *testing.T) {
	// 25 October 2026, end of daylight saving time in Europe: 02:30 CEST is
	// 00:30 UTC, 02:10 CET is 01:10 UTC. As local text the order is reversed.
	summer := time.Date(2026, 10, 25, 2, 30, 0, 0, cest)
	winter := time.Date(2026, 10, 25, 2, 10, 0, 0, cet)

	if got, want := dbTime(summer), "2026-10-25 00:30:00+00:00"; got != want {
		t.Fatalf("dbTime(summer) = %q, want %q", got, want)
	}
	if got, want := dbTime(winter), "2026-10-25 01:10:00+00:00"; got != want {
		t.Fatalf("dbTime(winter) = %q, want %q", got, want)
	}
	if !(dbTime(summer) < dbTime(winter)) {
		t.Fatal("UTC text must sort in time order across the DST switch")
	}

	// Trimmed fractional seconds still sort in time order.
	whole := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	frac := whole.Add(500 * time.Millisecond)
	next := whole.Add(time.Second)
	if !(dbTime(whole) < dbTime(frac) && dbTime(frac) < dbTime(next)) {
		t.Fatalf("fractional seconds out of order: %q %q %q", dbTime(whole), dbTime(frac), dbTime(next))
	}

	// The driver parses what dbTime writes back to the same instant.
	got, ok := parseStoredTime(dbTime(frac))
	if !ok || !got.Equal(frac) || got.Location() != time.UTC {
		t.Fatalf("round trip = %v (ok=%v), want %v in UTC", got, ok, frac)
	}
}

// openLegacyTimestampDB writes a messages.db the way older builds left it:
// local times with "+02:00" and "+01:00" offsets, plus a few odd values, and
// no user_version marker.
func openLegacyTimestampDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open legacy DB: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO chats (jid, name, last_message_time) VALUES
			('berlin@s.whatsapp.net', 'Berlin', '2026-10-25 02:10:00+01:00'),
			('null@s.whatsapp.net',   'NoTime', NULL);
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES
			('summer',  'berlin@s.whatsapp.net', 'berlin', 'before the switch', '2026-10-25 02:30:00+02:00', 0),
			('winter',  'berlin@s.whatsapp.net', 'berlin', 'after the switch',  '2026-10-25 02:10:00+01:00', 0),
			('midnight','berlin@s.whatsapp.net', 'berlin', 'local midnight',    '2026-10-20 01:30:00+02:00', 0),
			('zulu',    'berlin@s.whatsapp.net', 'berlin', 'z suffix',          '2026-10-20T08:00:00Z',      0),
			('bare',    'berlin@s.whatsapp.net', 'berlin', 'no offset',         '2026-10-20 09:00:00',       0),
			('junk',    'null@s.whatsapp.net',   'berlin', 'unparseable',       'not a time',                0);
		INSERT INTO poll_votes (poll_message_id, poll_chat_jid, voter_jid, selected_options_json, timestamp) VALUES
			('summer', 'berlin@s.whatsapp.net', 'voter@s.whatsapp.net', '["a"]', '2026-10-21 12:00:00.5+02:00');
	`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
	return dir
}

func rawColumn(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v.String
}

func TestMigrateSchema_NormalizesTimestampsToUTC(t *testing.T) {
	dir := openLegacyTimestampDB(t)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	rawMsg := func(id string) string {
		return rawColumn(t, s.db, "SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = ?", id)
	}
	for id, want := range map[string]string{
		"summer":   "2026-10-25 00:30:00+00:00",
		"winter":   "2026-10-25 01:10:00+00:00",
		"midnight": "2026-10-19 23:30:00+00:00",
		"zulu":     "2026-10-20 08:00:00+00:00",
		"bare":     "2026-10-20 09:00:00+00:00",
		"junk":     "not a time", // left alone
	} {
		if got := rawMsg(id); got != want {
			t.Errorf("messages[%s].timestamp = %q, want %q", id, got, want)
		}
	}
	if got, want := rawColumn(t, s.db, "SELECT CAST(last_message_time AS TEXT) FROM chats WHERE jid = 'berlin@s.whatsapp.net'"), "2026-10-25 01:10:00+00:00"; got != want {
		t.Errorf("chats.last_message_time = %q, want %q", got, want)
	}
	if got := rawColumn(t, s.db, "SELECT last_message_time FROM chats WHERE jid = 'null@s.whatsapp.net'"); got != "" {
		t.Errorf("NULL last_message_time became %q", got)
	}
	if got, want := rawColumn(t, s.db, "SELECT CAST(timestamp AS TEXT) FROM poll_votes"), "2026-10-21 10:00:00.5+00:00"; got != want {
		t.Errorf("poll_votes.timestamp = %q, want %q", got, want)
	}

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != schemaVersionUTCTimestamps {
		t.Fatalf("user_version = %d, want %d", version, schemaVersionUTCTimestamps)
	}

	// The DST pair now comes back in time order, newest first.
	msgs, err := s.ListMessages(context.Background(), ListMessagesParams{
		ChatJID: "berlin@s.whatsapp.net",
		After:   time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if got, want := orderedIDs(msgs), "winter,summer"; got != want {
		t.Fatalf("DST order = %s, want %s", got, want)
	}
	for _, m := range msgs {
		if m.Timestamp.Location() != time.UTC {
			t.Errorf("message %s timestamp in %v, want UTC", m.ID, m.Timestamp.Location())
		}
	}

	// The marker makes the rewrite run once: a row written in the old form
	// after the migration is not touched by a second run.
	if _, err := s.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('late', 'berlin@s.whatsapp.net', 'berlin', 'x', '2026-10-26 10:00:00+01:00', 0)`); err != nil {
		t.Fatalf("insert late row: %v", err)
	}
	if err := migrateSchema(s.db); err != nil {
		t.Fatalf("second migrateSchema: %v", err)
	}
	if got, want := rawMsg("late"), "2026-10-26 10:00:00+01:00"; got != want {
		t.Fatalf("second run rewrote a row: got %q, want %q", got, want)
	}
}

// A database without the poll_votes table (older than the poll feature) must
// still migrate.
func TestNormalizeTimestamps_MissingTable(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (id TEXT, chat_jid TEXT, timestamp TIMESTAMP);
		INSERT INTO messages VALUES ('m', 'c', '2026-07-01 10:00:00+02:00');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := normalizeTimestamps(db); err != nil {
		t.Fatalf("normalizeTimestamps: %v", err)
	}
	if got, want := rawColumn(t, db, "SELECT CAST(timestamp AS TEXT) FROM messages"), "2026-07-01 08:00:00+00:00"; got != want {
		t.Fatalf("timestamp = %q, want %q", got, want)
	}
}

// Messages arriving at local times between 00:00 and 02:00 belong to the
// previous UTC day. The filters must compare instants in UTC, whatever
// offset the caller's bound carries.
func TestListMessages_TimeFiltersAcrossDayBoundary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const chat = "447700000001@s.whatsapp.net"

	for _, m := range []Message{
		// 01:30 CEST on 20 October is 23:30 UTC on 19 October.
		{ID: "late-night", Timestamp: time.Date(2026, 10, 20, 1, 30, 0, 0, cest)},
		// 02:30 CEST is 00:30 UTC on 20 October.
		{ID: "early-morning", Timestamp: time.Date(2026, 10, 20, 2, 30, 0, 0, cest)},
	} {
		m.ChatJID, m.Sender, m.Content = chat, "447700000001", m.ID
		if err := s.StoreMessage(ctx, m, nil, nil, nil, 0); err != nil {
			t.Fatalf("StoreMessage %s: %v", m.ID, err)
		}
	}

	cases := []struct {
		name          string
		after, before time.Time
		want          string
	}{
		{"after UTC day start", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), time.Time{}, "early-morning"},
		{"after local day start", time.Date(2026, 10, 20, 0, 0, 0, 0, cest), time.Time{}, "early-morning,late-night"},
		{"before UTC day start", time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), "late-night"},
		{"after is inclusive", time.Date(2026, 10, 20, 0, 30, 0, 0, time.UTC), time.Time{}, "early-morning"},
		{"before is exclusive", time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 2, 30, 0, 0, cest), "late-night"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, err := s.ListMessages(ctx, ListMessagesParams{ChatJID: chat, After: tc.after, Before: tc.before, Limit: 50})
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			if got := strings.Join(messageIDs(msgs), ","); got != tc.want {
				t.Fatalf("ids = %s, want %s", got, tc.want)
			}
		})
	}
}

// A vote replayed with an older timestamp loses even when its local offset
// makes it sort later as text, and a newer vote wins the other way round.
func TestStorePollVote_ComparesInstantsAcrossOffsets(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const chat, voter = "123456789@g.us", "447700000002@s.whatsapp.net"

	// 12:00 CEST = 10:00 UTC.
	if err := s.StorePollVote(ctx, "p", chat, voter, []string{"first"}, time.Date(2026, 10, 21, 12, 0, 0, 0, cest)); err != nil {
		t.Fatalf("first vote: %v", err)
	}
	// 11:00 UTC is later although "11:00" < "12:00".
	if err := s.StorePollVote(ctx, "p", chat, voter, []string{"second"}, time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("second vote: %v", err)
	}
	// 12:30 CEST = 10:30 UTC, older than the stored vote.
	if err := s.StorePollVote(ctx, "p", chat, voter, []string{"replay"}, time.Date(2026, 10, 21, 12, 30, 0, 0, cest)); err != nil {
		t.Fatalf("replayed vote: %v", err)
	}
	got := rawColumn(t, s.db, "SELECT selected_options_json FROM poll_votes WHERE poll_message_id = 'p'")
	if got != `["second"]` {
		t.Fatalf("stored selection = %s, want [\"second\"]", got)
	}
}
