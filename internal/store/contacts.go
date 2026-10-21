package store

import (
	"database/sql"
	"strings"
)

// contactNameExpr coalesces the whatsmeow_contacts name columns in preference
// order: the saved address-book name first, then push name, first name, and
// business name. NULLIF folds empty strings into NULL so COALESCE skips them.
const contactNameExpr = `COALESCE(NULLIF(full_name,''), NULLIF(push_name,''), NULLIF(first_name,''), NULLIF(business_name,''))`

// ContactName returns the synced address-book name for a JID from the
// whatsmeow_contacts table, or "" when the contact is unknown or the
// whatsmeow store is unavailable. LID JIDs are resolved to phone JIDs first.
func (s *Store) ContactName(jid string) string {
	if s.whatsmeowDB == nil {
		return ""
	}
	jid = s.ResolveLIDToJID(jid)
	var name sql.NullString
	err := s.whatsmeowDB.QueryRow(
		`SELECT `+contactNameExpr+` FROM whatsmeow_contacts WHERE their_jid = ?`, jid).Scan(&name)
	if err != nil || !name.Valid {
		return ""
	}
	return name.String
}

// displayName picks the name shown for a chat: the synced address-book name
// when there is one, else the stored chat name unless it is just the bare
// phone number, else the phone number (JID user part). 1:1 chat names are
// often stored as the bare number, so the address book wins. Groups have no
// address-book entry and skip that lookup.
func (s *Store) displayName(jid, stored string) string {
	if !strings.HasSuffix(jid, "@g.us") {
		if name := s.ContactName(jid); name != "" {
			return name
		}
	}
	user := jidUser(jid)
	if stored != "" && stored != user {
		return stored
	}
	return user
}

// searchContactNames returns address-book contacts whose name or JID matches
// the LIKE pattern, as a jid -> display-name map, taking the first limit
// contacts in JID order (all of them when limit <= 0). Empty when the
// whatsmeow store is unavailable.
func (s *Store) searchContactNames(pattern string, limit int) map[string]string {
	out := map[string]string{}
	if s.whatsmeowDB == nil {
		return out
	}
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := s.whatsmeowDB.Query(`
		SELECT their_jid, `+contactNameExpr+` AS name
		FROM whatsmeow_contacts
		WHERE their_jid LIKE '%@s.whatsapp.net'
		  AND (LOWER(full_name) LIKE LOWER(?) OR LOWER(push_name) LIKE LOWER(?)
		    OR LOWER(first_name) LIKE LOWER(?) OR LOWER(business_name) LIKE LOWER(?)
		    OR their_jid LIKE ?)
		ORDER BY their_jid
		LIMIT ?`, pattern, pattern, pattern, pattern, pattern, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var jid string
		var name sql.NullString
		if err := rows.Scan(&jid, &name); err != nil {
			continue
		}
		if name.Valid && name.String != "" {
			out[jid] = name.String
		}
	}
	return out
}
