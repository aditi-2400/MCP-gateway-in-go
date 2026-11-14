package audit

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Fake secrets are built at runtime so no real-looking token sits in the
// source, where secret scanners would flag it.
var (
	gatewayKey  = "mcpgw_" + strings.Repeat("Ab3_", 10)
	openaiKey   = "sk-" + strings.Repeat("x9", 12)
	githubToken = "ghp_" + strings.Repeat("a1", 18)
	githubPAT   = "github_pat_" + strings.Repeat("B2", 15)
	awsKeyID    = "AKIA" + strings.Repeat("Q", 16)
	slackToken  = "xoxb-" + strings.Repeat("12", 8)
	pemKey      = "-----BEGIN RSA PRIVATE KEY-----\nMIIEow" + strings.Repeat("Q", 40) + "\n-----END RSA PRIVATE KEY-----"
)

// sameJSON compares two JSON texts by value, since key order may differ.
func sameJSON(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("output is not valid JSON: %s", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad test want %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestRedactArguments(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "nothing", in: "", want: "null"},
		{name: "null", in: "null", want: "null"},
		{name: "no secrets", in: `{"path":"/tmp/x","n":3,"ok":true,"tags":["a","b"]}`, want: `{"path":"/tmp/x","n":3,"ok":true,"tags":["a","b"]}`},

		// Sensitive names hide the whole value.
		{name: "password", in: `{"user":"ada","password":"hunter2"}`, want: `{"user":"ada","password":"[REDACTED]"}`},
		{name: "name with dashes and caps", in: `{"API-Key":"abc"}`, want: `{"API-Key":"[REDACTED]"}`},
		{name: "camel case", in: `{"apiKey":"abc"}`, want: `{"apiKey":"[REDACTED]"}`},
		{name: "name contains token", in: `{"x_auth_token":"abc"}`, want: `{"x_auth_token":"[REDACTED]"}`},
		{name: "nested header", in: `{"headers":{"Authorization":"Basic YWRhOmh1bnRlcjI="}}`, want: `{"headers":{"Authorization":"[REDACTED]"}}`},
		{name: "object under sensitive name", in: `{"credentials":{"user":"ada","pin":1234}}`, want: `{"credentials":"[REDACTED]"}`},
		{name: "number under sensitive name", in: `{"secret":42}`, want: `{"secret":"[REDACTED]"}`},
		{name: "author is not auth", in: `{"author":"ada"}`, want: `{"author":"ada"}`},

		// Secret-looking values are found under any name.
		{name: "gateway key", in: `{"note":` + quote("use "+gatewayKey+" here") + `}`, want: `{"note":"use [REDACTED] here"}`},
		{name: "bearer", in: `{"curl":"curl -H 'Authorization: Bearer abcdefgh.ijkl' x"}`, want: `{"curl":"curl -H 'Authorization: [REDACTED]' x"}`},
		{name: "key inside SQL", in: `{"sql":` + quote("SELECT * FROM t WHERE k='"+openaiKey+"'") + `}`, want: `{"sql":"SELECT * FROM t WHERE k='[REDACTED]'"}`},
		{name: "github token", in: `{"text":` + quote(githubToken) + `}`, want: `{"text":"[REDACTED]"}`},
		{name: "github fine-grained", in: `{"text":` + quote(githubPAT) + `}`, want: `{"text":"[REDACTED]"}`},
		{name: "aws key id", in: `{"text":` + quote("id "+awsKeyID) + `}`, want: `{"text":"id [REDACTED]"}`},
		{name: "slack token", in: `{"text":` + quote(slackToken) + `}`, want: `{"text":"[REDACTED]"}`},
		{name: "pem key", in: `{"file":` + quote(pemKey) + `}`, want: `{"file":"[REDACTED]-----END RSA PRIVATE KEY-----"}`},
		{name: "deep in arrays", in: `{"a":[{"b":{"c":["ok","Bearer abcdefghijk"]}}]}`, want: `{"a":[{"b":{"c":["ok","[REDACTED]"]}}]}`},
		{name: "top-level array", in: `["fine",` + quote(openaiKey) + `]`, want: `["fine","[REDACTED]"]`},
		{name: "key in an object name stays", in: `{"note":"sk-short"}`, want: `{"note":"sk-short"}`},

		// Card numbers, only when the Luhn check passes.
		{name: "card with spaces", in: `{"text":"card 4111 1111 1111 1111 ok"}`, want: `{"text":"card [REDACTED] ok"}`},
		{name: "card with dashes", in: `{"text":"5555-5555-5555-4444"}`, want: `{"text":"[REDACTED]"}`},
		{name: "amex", in: `{"text":"378282246310005"}`, want: `{"text":"[REDACTED]"}`},
		{name: "card as a JSON number", in: `{"pan":4111111111111111}`, want: `{"pan":"[REDACTED]"}`},
		{name: "fails Luhn", in: `{"order":"4111111111111112"}`, want: `{"order":"4111111111111112"}`},
		{name: "too short", in: `{"n":"411111111111"}`, want: `{"n":"411111111111"}`},
		{name: "big number kept exactly", in: `{"n":123456789012345678901234567890}`, want: `{"n":123456789012345678901234567890}`},

		// Never store text that isn't JSON.
		{name: "broken JSON", in: `{"password":"hunter2"`, want: `"[unreadable arguments]"`},
		{name: "two values", in: `{} {"password":"hunter2"}`, want: `"[unreadable arguments]"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactArguments(json.RawMessage(tt.in))
			if !sameJSON(t, got, tt.want) {
				t.Errorf("RedactArguments(%s)\n got %s\nwant %s", tt.in, got, tt.want)
			}
		})
	}
}

// Secrets with a recognizable shape are hidden under any name. A plain
// password like "hunter2" is only hidden under a sensitive name (tested
// above): nothing about the text itself says it is a secret.
func TestRedactNeverLeaksKnownSecrets(t *testing.T) {
	for _, secret := range []string{gatewayKey, openaiKey, githubToken, githubPAT, awsKeyID, slackToken} {
		in := `{"a":` + quote(secret) + `,"password":` + quote(secret) + `,"list":[` + quote("x "+secret+" y") + `]}`
		if out := string(RedactArguments(json.RawMessage(in))); strings.Contains(out, secret) {
			t.Errorf("output %s still contains %q", out, secret)
		}
	}
}

func TestIsCardNumber(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"4111111111111111", true},
		{"4111 1111 1111 1111", true},
		{"4111-1111-1111-1111", true},
		{"378282246310005", true},
		{"6011111111111117", true},
		{"4111111111111112", false},
		{"411111111111", false},         // 12 digits
		{"41111111111111111111", false}, // 20 digits
		{"4111a111111111111", false},    // not all digits
		{"", false},
	}
	for _, tt := range tests {
		if got := isCardNumber(tt.in); got != tt.want {
			t.Errorf("isCardNumber(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// The output must always be valid JSON, whatever comes in.
func FuzzRedactArguments(f *testing.F) {
	for _, seed := range []string{`{}`, `{"password":"x"}`, `[1,"Bearer abcdefghij"]`, `{"a":4111111111111111}`, `{`, `"\ud800"`, `null`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := RedactArguments(json.RawMessage(in))
		if !json.Valid(out) {
			t.Fatalf("RedactArguments(%q) = %q, not valid JSON", in, out)
		}
	})
}
