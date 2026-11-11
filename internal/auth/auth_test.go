package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGenerateKeyFormat(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		key, err := GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey() error = %v", err)
		}
		if !strings.HasPrefix(key, KeyPrefix) || len(key) != len(KeyPrefix)+43 {
			t.Fatalf("key %q: want %s plus 43 base64url characters", key, KeyPrefix)
		}
		if strings.ContainsAny(key, "+/=") {
			t.Fatalf("key %q is not unpadded base64url", key)
		}
		if seen[key] {
			t.Fatalf("GenerateKey() repeated %q", key)
		}
		seen[key] = true
	}
}

func TestHashKeyIsSHA256Hex(t *testing.T) {
	// The standard SHA-256 test vector for "abc" (FIPS 180-2, appendix B.1).
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashKey("abc"); got != want {
		t.Fatalf(`HashKey("abc") = %s, want %s`, got, want)
	}
}

func newAuth(t *testing.T, entries ...Entry) *Authenticator {
	t.Helper()
	a, err := New(entries)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return a
}

func mustKey(t *testing.T) string {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestAuthenticate(t *testing.T) {
	ciKey, devKey := mustKey(t), mustKey(t)
	a := newAuth(t,
		Entry{Name: "ci-bot", KeySHA256: HashKey(ciKey)},
		Entry{Name: "dev", KeySHA256: HashKey(devKey)},
	)

	tests := []struct {
		name    string
		key     string
		want    string
		wantErr error
	}{
		{name: "first agent", key: ciKey, want: "ci-bot"},
		{name: "second agent", key: devKey, want: "dev"},
		{name: "no key", key: "", wantErr: ErrNoKey},
		{name: "unknown key", key: mustKey(t), wantErr: ErrInvalidKey},
		{name: "wrong prefix", key: "sk_" + strings.TrimPrefix(ciKey, KeyPrefix), wantErr: ErrInvalidKey},
		{name: "truncated", key: ciKey[:len(ciKey)-1], wantErr: ErrInvalidKey},
		{name: "extra character", key: ciKey + "A", wantErr: ErrInvalidKey},
		{name: "different case", key: strings.ToUpper(ciKey), wantErr: ErrInvalidKey},
		{name: "surrounding space", key: " " + ciKey + " ", wantErr: ErrInvalidKey},
		{name: "the hash itself", key: HashKey(ciKey), wantErr: ErrInvalidKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, err := a.Authenticate(tt.key)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Authenticate() error = %v, want %v", err, tt.wantErr)
				}
				if agent != (Agent{}) {
					t.Errorf("Authenticate() agent = %+v on error, want zero", agent)
				}
				return
			}
			if err != nil || agent.Name != tt.want {
				t.Fatalf("Authenticate() = %+v, %v, want %s", agent, err, tt.want)
			}
		})
	}
}

func TestAuthenticateErrorsDoNotIncludeTheKey(t *testing.T) {
	a := newAuth(t, Entry{Name: "ci-bot", KeySHA256: HashKey(mustKey(t))})
	key := mustKey(t)
	if _, err := a.Authenticate(key); err == nil || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), HashKey(key)) {
		t.Fatalf("error %q must not contain the key or its hash", err)
	}
}

func TestNewRejectsBadEntries(t *testing.T) {
	good := HashKey(mustKey(t))
	tests := []struct {
		name    string
		entries []Entry
	}{
		{name: "no agents", entries: nil},
		{name: "empty name", entries: []Entry{{Name: "", KeySHA256: good}}},
		{name: "short hash", entries: []Entry{{Name: "a", KeySHA256: good[:63]}}},
		{name: "uppercase hash", entries: []Entry{{Name: "a", KeySHA256: strings.ToUpper(good)}}},
		{name: "not hex", entries: []Entry{{Name: "a", KeySHA256: strings.Repeat("z", 64)}}},
		{name: "raw key in place of hash", entries: []Entry{{Name: "a", KeySHA256: mustKey(t)}}},
		{name: "duplicate name", entries: []Entry{{Name: "a", KeySHA256: good}, {Name: "a", KeySHA256: HashKey(mustKey(t))}}},
		{name: "same key for two agents", entries: []Entry{{Name: "a", KeySHA256: good}, {Name: "b", KeySHA256: good}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.entries); err == nil {
				t.Fatal("New() error = nil, want an error")
			}
		})
	}
}

func TestParseBearer(t *testing.T) {
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{header: "Bearer mcpgw_abc", want: "mcpgw_abc", ok: true},
		{header: "bearer mcpgw_abc", want: "mcpgw_abc", ok: true},
		{header: "BEARER mcpgw_abc", want: "mcpgw_abc", ok: true},
		{header: "", ok: false},
		{header: "Bearer", ok: false},
		{header: "Bearer ", ok: false},
		{header: "Basic dXNlcjpwYXNz", ok: false},
		{header: "mcpgw_abc", ok: false},
		{header: "Bearer mcpgw_abc extra", ok: false},
		{header: "Bearer  mcpgw_abc", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			got, ok := ParseBearer(tt.header)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("ParseBearer(%q) = %q, %v, want %q, %v", tt.header, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestAgentInContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("FromContext(empty) ok = true, want false")
	}
	ctx := WithAgent(context.Background(), Agent{Name: "ci-bot"})
	if agent, ok := FromContext(ctx); !ok || agent.Name != "ci-bot" {
		t.Fatalf("FromContext() = %+v, %v", agent, ok)
	}
}
