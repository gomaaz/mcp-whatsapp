package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// timestampLayout is the text form of every timestamp the store writes: UTC
// with an explicit "+00:00" offset, which is also the layout go-sqlite3 binds
// a time.Time with, so the driver's scan code parses it unchanged. With one
// offset for every row, text order is time order, so the plain column
// comparisons and ORDER BY clauses in the queries are correct. Trailing zeros
// of the fractional second are trimmed; that still sorts correctly because
// '+' sorts before every digit.
const timestampLayout = "2006-01-02 15:04:05.999999999-07:00"

// dbTime renders t in timestampLayout. Every timestamp bound into a query,
// written or compared, goes through it: binding a time.Time directly would
// keep its local offset ("+02:00" in summer, "+01:00" in winter) and break
// text comparisons against the stored rows.
func dbTime(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}

// parseStoredTime parses a timestamp column value the way go-sqlite3 does when
// it scans a TIMESTAMP column: a trailing "Z" is dropped, every layout in
// sqlite3.SQLiteTimestampFormats is tried, and a value without an offset is
// read as UTC.
func parseStoredTime(s string) (time.Time, bool) {
	s = strings.TrimSuffix(s, "Z")
	for _, layout := range sqlite3.SQLiteTimestampFormats {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// schemaVersionUTCTimestamps is the PRAGMA user_version from which every
// timestamp column holds UTC text in timestampLayout.
const schemaVersionUTCTimestamps = 1

// timestampColumns lists every timestamp column the store owns.
var timestampColumns = []struct{ table, column string }{
	{"messages", "timestamp"},
	{"chats", "last_message_time"},
	{"poll_votes", "timestamp"},
}

// normalizeTimestamps rewrites timestamps that older builds stored with the
// local offset of the machine ("2026-10-20 11:27:49+02:00") to UTC text in
// timestampLayout, so that rows written before and after the upgrade compare
// correctly as text. It runs once, in one transaction, and then records
// schemaVersionUTCTimestamps in PRAGMA user_version. Values it cannot parse
// are left untouched; rewriting a value that is already UTC is a no-op, so the
// step stays idempotent even without the marker.
func normalizeTimestamps(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version >= schemaVersionUTCTimestamps {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin timestamp migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, c := range timestampColumns {
		if err := normalizeTimestampColumn(tx, c.table, c.column); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersionUTCTimestamps)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit timestamp migration: %w", err)
	}
	return nil
}

// normalizeTimestampColumn rewrites one column. A table that does not exist
// yet (a legacy database without poll_votes) is skipped.
func normalizeTimestampColumn(tx *sql.Tx, table, column string) error {
	var exists int
	if err := tx.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)", table,
	).Scan(&exists); err != nil {
		return fmt.Errorf("look up table %s: %w", table, err)
	}
	if exists == 0 {
		return nil
	}

	// CAST hides the TIMESTAMP declared type, so the driver hands back the raw
	// text instead of a parsed time.Time (which would turn an unparseable
	// value into the zero time).
	rows, err := tx.Query(fmt.Sprintf(
		"SELECT rowid, CAST(%[1]s AS TEXT) FROM %[2]s WHERE typeof(%[1]s) = 'text'", column, table))
	if err != nil {
		return fmt.Errorf("read %s.%s: %w", table, column, err)
	}
	type update struct {
		rowID int64
		value string
	}
	var updates []update
	for rows.Next() {
		var (
			rowID int64
			raw   string
		)
		if err := rows.Scan(&rowID, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("scan %s.%s: %w", table, column, err)
		}
		t, ok := parseStoredTime(raw)
		if !ok {
			continue
		}
		if v := dbTime(t); v != raw {
			updates = append(updates, update{rowID, v})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate %s.%s: %w", table, column, err)
	}
	rows.Close()

	if len(updates) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(fmt.Sprintf("UPDATE %s SET %s = ? WHERE rowid = ?", table, column))
	if err != nil {
		return fmt.Errorf("prepare %s.%s update: %w", table, column, err)
	}
	defer stmt.Close()
	for _, u := range updates {
		if _, err := stmt.Exec(u.value, u.rowID); err != nil {
			return fmt.Errorf("update %s.%s: %w", table, column, err)
		}
	}
	return nil
}
