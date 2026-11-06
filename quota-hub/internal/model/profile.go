package model

import (
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProfileText rejects rather than truncates untrusted metadata. Controls are
// checked before trimming, including newlines, and invalid UTF-8 is rejected.
func ProfileText(s string, max int) *string {
	if !utf8.ValidString(s) {
		return nil
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return nil
		}
	}
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > max {
		return nil
	}
	return &s
}

func Email(s string) *string {
	p := ProfileText(s, 300)
	if p == nil {
		return nil
	}
	a, err := mail.ParseAddress(*p)
	if err != nil || a.Address != *p || !strings.Contains(a.Address, "@") {
		return nil
	}
	return p
}
