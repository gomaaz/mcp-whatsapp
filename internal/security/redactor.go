package security

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// phoneDigitRun matches international-format phone numbers: an optional "+"
// followed by 10 or more digits. Used to mask phone numbers in debug output.
var phoneDigitRun = regexp.MustCompile(`\+?\d{10,}`)

// Redactor obscures JIDs and message bodies in log output. A zero-value
// Redactor redacts. Setting Debug=true passes values through unchanged so
// developers can trace content during active debugging.
//
// Construct once at startup and thread through. The struct is read-only
// after construction; no locking required.
type Redactor struct {
	Debug bool
}

// JID returns "…" + up to the last 4 characters of the user-part of jid
// (the portion before "@"). Empty input returns "…".
//
// Debug=true: if the user-part matches the phone digit-run pattern, the
// user-part is masked to "*****<last5>" with the @server suffix preserved.
// Short user-parts (< 10 digits) pass through unchanged.
//
// Note: this is obfuscation for log-reader convenience, not anonymisation.
// Someone with independent knowledge of the user's contacts can still
// correlate "…4567" with a specific phone number.
func (r *Redactor) JID(jid string) string {
	user, suffix := splitJID(jid)
	if r.Debug {
		if phoneDigitRun.MatchString(user) {
			return "*****" + last5(user) + suffix
		}
		return jid
	}
	if user == "" {
		return "…"
	}
	runes := []rune(user)
	if len(runes) > 4 {
		return "…" + string(runes[len(runes)-4:])
	}
	return "…" + user
}

// splitJID splits jid at "@" into its user-part and "@"-prefixed suffix. If
// jid contains no "@", suffix is "".
func splitJID(jid string) (user, suffix string) {
	if i := strings.Index(jid, "@"); i >= 0 {
		return jid[:i], jid[i:]
	}
	return jid, ""
}

// Body returns a fixed-shape summary "[<len>B: text|url|command]" with phone
// digit runs masked in both paths.
//
// Non-debug: returns the summary string with any phone digit runs masked.
// Debug: returns the full body with phone digit runs masked to "****<last5>".
//
// Classifier (non-debug path): strings starting with http:// or https:// are
// "url"; strings starting with "/" or "!" are "command"; everything else
// (including the empty string) is "text".
func (r *Redactor) Body(content string) string {
	if r.Debug {
		return maskPhones(content)
	}
	kind := "text"
	switch {
	case strings.HasPrefix(content, "http://"), strings.HasPrefix(content, "https://"):
		kind = "url"
	case strings.HasPrefix(content, "/"), strings.HasPrefix(content, "!"):
		kind = "command"
	}
	return fmt.Sprintf("[%dB: %s]", len(content), kind)
}

// URL redacts a media CDN URL. Non-debug: returns "<scheme>://<host>/…" if
// parseable, otherwise "[url]". Debug: returns the full URL.
func (r *Redactor) URL(raw string) string {
	if r.Debug {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[url]"
	}
	return fmt.Sprintf("%s://%s/…", u.Scheme, u.Host)
}

// MsgID redacts a message ID for log output. Non-debug: "…" + last 6 chars.
// Debug: returns the full ID unchanged.
func (r *Redactor) MsgID(id string) string {
	if r.Debug {
		return id
	}
	if id == "" {
		return "…"
	}
	runes := []rune(id)
	if len(runes) > 6 {
		return "…" + string(runes[len(runes)-6:])
	}
	return "…" + id
}

// Name redacts a contact, push, group or file name for log output.
// Non-debug: only whether a name is present and its length, as
// "[name: <n> chars]" or "[no name]". Debug: the name with phone digit runs
// masked, as in Body.
func (r *Redactor) Name(name string) string {
	if r.Debug {
		return maskPhones(name)
	}
	if name == "" {
		return "[no name]"
	}
	return fmt.Sprintf("[name: %d chars]", utf8.RuneCountInString(name))
}

// jidInText matches a WhatsApp JID inside free text: a user part with an
// optional ".agent" and ":device" suffix, "@" and one of the WhatsApp
// servers. Ordinary e-mail addresses do not match.
var jidInText = regexp.MustCompile(`[0-9A-Za-z_+-]+(?:\.\d+)?(?::\d+)?@(?:s\.whatsapp\.net|c\.us|lid|g\.us|broadcast|newsletter|hosted\.lid|hosted|bot|interop|msgr|call)\b`)

// Line scrubs a finished log line, as a safety net for text the call site
// did not redact itself, such as whatsmeow's own log lines and error values.
// Non-debug: every JID becomes JID() of its user part (without agent and
// device), and every remaining run of 10 or more digits becomes "…" + its
// last 4 digits. Debug: the line is returned unchanged.
//
// Line cannot recognise names or message text; call sites must still pass
// those through Name and Body.
func (r *Redactor) Line(s string) string {
	if r.Debug {
		return s
	}
	s = jidInText.ReplaceAllStringFunc(s, func(match string) string {
		user, _ := splitJID(match)
		if i := strings.IndexAny(user, ".:"); i >= 0 {
			user = user[:i]
		}
		return r.JID(user)
	})
	return phoneDigitRun.ReplaceAllStringFunc(s, func(match string) string {
		digits := strings.TrimPrefix(match, "+")
		return "…" + digits[len(digits)-4:]
	})
}

// maskPhones replaces phone digit runs with "****" + last 5 characters.
func maskPhones(s string) string {
	return phoneDigitRun.ReplaceAllStringFunc(s, func(match string) string {
		return "****" + last5(match)
	})
}

// last5 returns the last 5 characters of s, ignoring a leading "+".
func last5(s string) string {
	digits := s
	if strings.HasPrefix(digits, "+") {
		digits = digits[1:]
	}
	if len(digits) <= 5 {
		return digits
	}
	return digits[len(digits)-5:]
}
