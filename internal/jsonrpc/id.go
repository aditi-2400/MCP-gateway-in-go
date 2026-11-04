package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

// ID is a JSON-RPC request ID. MCP allows a string or an integer and forbids null.
//
// ID is a plain comparable struct so it can be used as a map key when matching
// responses to pending requests. The string "1" and the integer 1 are different IDs.
type ID struct {
	str   string
	num   int64
	isStr bool
}

// NewIntID returns an integer ID.
func NewIntID(n int64) ID { return ID{num: n} }

// NewStringID returns a string ID.
func NewStringID(s string) ID { return ID{str: s, isStr: true} }

// IsString reports whether the ID was a JSON string.
func (id ID) IsString() bool { return id.isStr }

// String returns the ID as it appears in JSON, for logs and error messages.
func (id ID) String() string {
	if id.isStr {
		return strconv.Quote(id.str)
	}
	return strconv.FormatInt(id.num, 10)
}

// MarshalJSON writes the ID as a JSON string or number.
func (id ID) MarshalJSON() ([]byte, error) {
	if id.isStr {
		return json.Marshal(id.str)
	}
	return []byte(strconv.FormatInt(id.num, 10)), nil
}

// UnmarshalJSON accepts a JSON string or an integer that fits in int64.
// Null, fractions, exponents, booleans, objects, and arrays are rejected.
func (id *ID) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return errors.New("id is empty")
	}
	if bytes.Equal(data, []byte("null")) {
		return errors.New("id must not be null")
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*id = NewStringID(s)
		return nil
	}
	// ParseInt only accepts an optional sign and digits, so 1.5, 1e3, and
	// true all fail here, as do numbers too large for int64.
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return errors.New("id must be a string or an integer")
	}
	*id = NewIntID(n)
	return nil
}
