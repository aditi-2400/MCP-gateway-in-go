// Package audit records every tools/call in an append-only log, with
// secrets taken out of the arguments first.
package audit

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Redacted replaces every secret the redactor finds.
const Redacted = "[REDACTED]"

// unreadable is stored in place of arguments that are not valid JSON. The
// raw text is never stored, since it could hold a secret.
const unreadable = `"[unreadable arguments]"`

// sensitiveNames are parts of argument names whose values are always
// hidden, whatever they look like. Names are compared in lower case with
// "-" and "_" removed, so API-Key, api_key, and apiKey all match "apikey".
var sensitiveNames = []string{
	"password", "passwd", "passphrase",
	"secret",
	"token",
	"apikey",
	"authorization",
	"cookie",
	"privatekey",
	"credential",
	"sessionid",
}

// secretPatterns find secrets inside any string, whatever the argument is
// called. Only the matching part is replaced, so the rest of a log message
// or SQL query stays readable.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`mcpgw_[A-Za-z0-9_-]{16,}`),                // this gateway's API keys
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),   // Authorization header values
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),                 // OpenAI- and Anthropic-style keys
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),            // GitHub tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),          // GitHub fine-grained tokens
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                    // AWS access key IDs
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),          // Slack tokens
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[^-]*`), // PEM private keys
}

// cardLike finds 13 to 19 digits, optionally split by single spaces or
// dashes, like card numbers are written. Matches are only redacted if they
// pass the Luhn check, so order IDs and timestamps mostly survive.
var cardLike = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)

// RedactArguments returns a copy of a tools/call "arguments" value with
// secrets replaced by Redacted. nil or null stays null. Arguments that are
// not valid JSON come back as a fixed marker, never as the raw text.
func RedactArguments(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("null")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keeps big numbers exact, so card numbers sent as numbers are seen whole
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return json.RawMessage(unreadable)
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return json.RawMessage(unreadable)
	}
	return out
}

func redactValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if isSensitiveName(k) {
				v[k] = Redacted
			} else {
				v[k] = redactValue(child)
			}
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = redactValue(child)
		}
		return v
	case string:
		return RedactString(v)
	case json.Number:
		if isCardNumber(v.String()) {
			return Redacted
		}
		return v
	default:
		return v
	}
}

func isSensitiveName(name string) bool {
	n := strings.ToLower(name)
	n = strings.NewReplacer("-", "", "_", "").Replace(n)
	for _, s := range sensitiveNames {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// RedactString replaces secrets found inside s.
func RedactString(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return cardLike.ReplaceAllStringFunc(s, func(m string) string {
		if isCardNumber(m) {
			return Redacted
		}
		return m
	})
}

// isCardNumber reports whether s is 13 to 19 digits (spaces and dashes
// allowed between them) that pass the Luhn checksum card numbers use.
func isCardNumber(s string) bool {
	digits := make([]int, 0, len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits = append(digits, int(r-'0'))
		case r == ' ' || r == '-':
		default:
			return false
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	// Luhn: from the right, double every second digit, subtract 9 if over
	// 9, and the total must be a multiple of 10.
	sum := 0
	for i := range digits {
		d := digits[len(digits)-1-i]
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
