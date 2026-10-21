package security

import "testing"

func TestRedactor_JID(t *testing.T) {
	r := &Redactor{}
	cases := map[string]string{
		"15551234567@s.whatsapp.net": "…4567",
		"123@s.whatsapp.net":         "…123",
		"":                           "…",
		"abc":                        "…abc",
		"12345":                      "…2345",
		"120363040000000000@g.us":    "…0000",
		"abcdef@lid":                 "…cdef",
		"🔥ab@s.whatsapp.net":         "…🔥ab",
		"a🔥bcd@s.whatsapp.net":       "…🔥bcd",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := r.JID(in); got != want {
				t.Fatalf("JID(%q): want %q, got %q", in, want, got)
			}
		})
	}
}

func TestRedactor_JID_Debug(t *testing.T) {
	r := &Redactor{Debug: true}
	// Phone-number user-parts are masked in debug mode.
	in := "15551234567@s.whatsapp.net"
	want := "*****34567@s.whatsapp.net"
	if got := r.JID(in); got != want {
		t.Fatalf("JID(%q): want %q, got %q", in, want, got)
	}
}

func TestRedactor_Body(t *testing.T) {
	r := &Redactor{}
	cases := map[string]string{
		"":                    "[0B: text]",
		"hello world":         "[11B: text]",
		"https://example.com": "[19B: url]",
		"http://example.com":  "[18B: url]",
		"/ping":               "[5B: command]",
		"!invite":             "[7B: command]",
		"hey /slash":          "[10B: text]",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := r.Body(in); got != want {
				t.Fatalf("Body(%q): want %q, got %q", in, want, got)
			}
		})
	}
}

func TestRedactor_Body_Debug(t *testing.T) {
	r := &Redactor{Debug: true}
	in := "hello world"
	if got := r.Body(in); got != in {
		t.Fatalf("debug mode should pass through, got %q", got)
	}
}

func TestRedactor_Body_DebugMasksPhones(t *testing.T) {
	r := &Redactor{Debug: true}
	in := "Call me on +15551234567"
	want := "Call me on ****34567"
	if got := r.Body(in); got != want {
		t.Fatalf("Body(%q): want %q, got %q", in, want, got)
	}
}

func TestRedactor_URL_NonDebug(t *testing.T) {
	r := &Redactor{}
	cases := map[string]string{
		"https://mmg.whatsapp.net/v/abc123": "https://mmg.whatsapp.net/…",
		"http://example.com/path?q=1":       "http://example.com/…",
		"not-a-url":                         "[url]",
		"":                                  "[url]",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := r.URL(in); got != want {
				t.Fatalf("URL(%q): want %q, got %q", in, want, got)
			}
		})
	}
}

func TestRedactor_URL_Debug(t *testing.T) {
	r := &Redactor{Debug: true}
	in := "https://mmg.whatsapp.net/v/abc123?token=secret"
	if got := r.URL(in); got != in {
		t.Fatalf("debug mode should pass through, got %q", got)
	}
}

func TestRedactor_JID_DebugLast5(t *testing.T) {
	r := &Redactor{Debug: true}
	in := "15551234567@s.whatsapp.net"
	want := "*****34567@s.whatsapp.net"
	if got := r.JID(in); got != want {
		t.Fatalf("JID(%q): want %q, got %q", in, want, got)
	}
}

func TestRedactor_JID_DebugShortPassesThrough(t *testing.T) {
	r := &Redactor{Debug: true}
	in := "abcd@s.whatsapp.net"
	if got := r.JID(in); got != in {
		t.Fatalf("short user-part should pass through in debug, got %q", got)
	}
}

func TestRedactor_MsgID_NonDebug(t *testing.T) {
	r := &Redactor{}
	cases := map[string]string{
		"":                 "…",
		"ABC":              "…ABC",
		"ABCDEF":           "…ABCDEF",
		"ABCDEFG":          "…BCDEFG",
		"3EB0123456789ABC": "…789ABC",
		"3EB0ABCDE12345F6": "…2345F6",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := r.MsgID(in); got != want {
				t.Fatalf("MsgID(%q): want %q, got %q", in, want, got)
			}
		})
	}
}

func TestRedactor_MsgID_Debug(t *testing.T) {
	r := &Redactor{Debug: true}
	in := "3EB0123456789ABCDEF"
	if got := r.MsgID(in); got != in {
		t.Fatalf("MsgID debug mode should pass through, got %q", got)
	}
}

func TestRedactor_MsgID_EmptyDebug(t *testing.T) {
	r := &Redactor{Debug: true}
	if got := r.MsgID(""); got != "" {
		t.Fatalf("MsgID debug mode empty string should be empty, got %q", got)
	}
}

func TestRedactor_Name(t *testing.T) {
	r := &Redactor{}
	cases := map[string]string{
		"":                     "[no name]",
		"Karin Müller":         "[name: 12 chars]",
		"Rechnung_Mueller.pdf": "[name: 20 chars]",
	}
	for in, want := range cases {
		if got := r.Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactor_Name_Debug(t *testing.T) {
	r := &Redactor{Debug: true}
	if got := r.Name("Karin Müller"); got != "Karin Müller" {
		t.Errorf("Name = %q, want the name unchanged", got)
	}
	if got := r.Name("+491751234567"); got != "****34567" {
		t.Errorf("Name = %q, want phone digits masked", got)
	}
}

func TestRedactor_Line(t *testing.T) {
	r := &Redactor{}
	cases := map[string]string{
		"Error decrypting message 3EB0AA from 491751234567:12@s.whatsapp.net: bad mac":    "Error decrypting message 3EB0AA from …4567: bad mac",
		"Migrated 3 sessions from 491751234567.0:3@s.whatsapp.net to 123456789012345@lid": "Migrated 3 sessions from …4567 to …2345",
		"Error parsing group 120363012345678901@g.us: x":                                  "Error parsing group …8901: x",
		"Updated LID to 123456789012345@lid":                                              "Updated LID to …2345",
		"call +491751234567 or 4917512345678":                                             "call …4567 or …5678",
		"Uploading 812 new prekeys (123456789 bytes)":                                     "Uploading 812 new prekeys (123456789 bytes)",
		"mail kunde@example.com":                                                          "mail kunde@example.com",
	}
	for in, want := range cases {
		if got := r.Line(in); got != want {
			t.Errorf("Line(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestRedactor_Line_Debug(t *testing.T) {
	r := &Redactor{Debug: true}
	in := "Error decrypting message from 491751234567:12@s.whatsapp.net"
	if got := r.Line(in); got != in {
		t.Errorf("Line = %q, want the line unchanged", got)
	}
}
