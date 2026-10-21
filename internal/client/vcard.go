package client

import "strings"

// buildVCard returns a minimal RFC 6350-style vCard 3.0 string for the given
// name and phone. Digits of phone are preserved as a WhatsApp-aware TEL line
// (`TEL;TYPE=CELL;waid=<digits>:+<digits>`). Returned bytes use CRLF line
// endings per the vCard spec.
func buildVCard(name, phone string) string {
	digits := digitsOnly(phone)
	displayName := name
	if displayName == "" {
		if digits != "" {
			displayName = "+" + digits
		} else {
			displayName = ""
		}
	}

	escapedFN := escapeVCardText(displayName)
	// N field: Family;Given;Additional;Prefix;Suffix. Dump the whole thing into
	// Family to keep it simple but spec-compliant.
	escapedN := escapeVCardText(displayName) + ";;;;"

	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\n")
	b.WriteString("VERSION:3.0\r\n")
	b.WriteString("FN:")
	b.WriteString(escapedFN)
	b.WriteString("\r\n")
	b.WriteString("N:")
	b.WriteString(escapedN)
	b.WriteString("\r\n")
	if digits != "" {
		b.WriteString("TEL;TYPE=CELL;waid=")
		b.WriteString(digits)
		b.WriteString(":+")
		b.WriteString(digits)
		b.WriteString("\r\n")
	}
	b.WriteString("END:VCARD\r\n")
	return b.String()
}

// digitsOnly returns s with every non-digit character removed.
func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r < '0' || r > '9' {
			return -1
		}
		return r
	}, s)
}

// parseVCard reads the name and phone numbers out of a received vCard (2.1,
// 3.0 or 4.0). name is the FN value, or "Given Family" from N when FN is
// missing. tels lists the TEL values in card order without duplicates; a
// TEL with an empty value falls back to "+<waid>". Folded lines are joined
// and group prefixes ("item1.TEL") ignored; anything it cannot read is
// skipped.
func parseVCard(card string) (name string, tels []string) {
	var structured string
	seen := make(map[string]bool)
	for _, line := range unfoldVCard(card) {
		head, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		prop, params, _ := strings.Cut(head, ";")
		if i := strings.LastIndex(prop, "."); i >= 0 {
			prop = prop[i+1:]
		}
		switch strings.ToUpper(strings.TrimSpace(prop)) {
		case "FN":
			if name == "" {
				name = strings.Join(strings.Fields(unescapeVCardText(value)), " ")
			}
		case "N":
			if structured == "" {
				fields := strings.Split(value, ";")
				var parts []string
				// N is Family;Given;Additional;Prefix;Suffix: print Given first.
				for _, i := range []int{1, 0} {
					if i < len(fields) {
						if f := strings.TrimSpace(unescapeVCardText(fields[i])); f != "" {
							parts = append(parts, f)
						}
					}
				}
				structured = strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
			}
		case "TEL":
			tel := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "tel:"))
			if tel == "" {
				tel = waidParam(params)
			}
			if tel != "" && !seen[tel] {
				seen[tel] = true
				tels = append(tels, tel)
			}
		}
	}
	if name == "" {
		name = structured
	}
	return name, tels
}

// unfoldVCard splits a vCard into logical lines, joining folded
// continuation lines (starting with a space or tab) onto the line before.
func unfoldVCard(card string) []string {
	var lines []string
	for _, raw := range strings.Split(strings.ReplaceAll(card, "\r\n", "\n"), "\n") {
		raw = strings.TrimRight(raw, "\r")
		if (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) && len(lines) > 0 {
			lines[len(lines)-1] += raw[1:]
			continue
		}
		if raw != "" {
			lines = append(lines, raw)
		}
	}
	return lines
}

// waidParam returns "+<digits>" for a TEL line's waid=<digits> parameter,
// or "" when there is none.
func waidParam(params string) string {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "waid") {
			if d := digitsOnly(v); d != "" {
				return "+" + d
			}
		}
	}
	return ""
}

// unescapeVCardText reverses escapeVCardText, except that an escaped line
// break becomes a space so the value stays on one line.
func unescapeVCardText(s string) string {
	replacer := strings.NewReplacer(
		"\\\\", "\\",
		"\\n", " ",
		"\\N", " ",
		"\\,", ",",
		"\\;", ";",
	)
	return replacer.Replace(s)
}

// escapeVCardText applies the RFC 6350 text-value escaping rules for the
// characters that are significant in the vCard grammar.
func escapeVCardText(s string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\n", "\\n",
		"\r", "",
		",", "\\,",
		";", "\\;",
	)
	return replacer.Replace(s)
}
