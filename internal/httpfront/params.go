package httpfront

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// A tool's inputSchema can mark a property with "x-mcp-header": "Region",
// which means clients copy that argument into an Mcp-Param-Region header so
// proxies can route on it. The gateway reads the body, so the spec requires
// it to check that the headers and the body agree.

const paramHeaderPrefix = "Mcp-Param-"

// paramHeader is one x-mcp-header annotation.
type paramHeader struct {
	name     string   // the part after Mcp-Param-
	path     []string // property names from the schema root
	jsonType string   // "string", "integer", or "boolean"
}

// paramHeaders returns the annotations in a tool's inputSchema, sorted by
// name. It returns an error if the tool definition breaks the spec's rules,
// in which case no call to the tool can be checked.
func paramHeaders(schema json.RawMessage) ([]paramHeader, error) {
	var root any
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode inputSchema: %w", err)
	}

	var found []paramHeader
	if err := walkSchema(root, nil, true, &found); err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })

	seen := make(map[string]bool, len(found))
	for _, p := range found {
		key := strings.ToLower(p.name)
		if seen[key] {
			return nil, fmt.Errorf("x-mcp-header %q is not unique (names are compared ignoring case)", p.name)
		}
		seen[key] = true
	}
	return found, nil
}

// walkSchema visits every part of a schema. A property is "statically
// reachable" only through a chain of properties keys from the root; an
// annotation anywhere else (items, oneOf, $defs, the root itself) makes the
// tool definition invalid.
func walkSchema(node any, path []string, isProperty bool, found *[]paramHeader) error {
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			if err := walkSchema(item, path, false, found); err != nil {
				return err
			}
		}
	case map[string]any:
		if raw, ok := v["x-mcp-header"]; ok {
			if !isProperty || len(path) == 0 {
				return errors.New("x-mcp-header is only allowed on properties reachable from the root through properties keys")
			}
			p, err := newParamHeader(raw, v["type"], path)
			if err != nil {
				return err
			}
			*found = append(*found, p)
		}
		for key, child := range v {
			if key == "properties" && isProperty {
				props, ok := child.(map[string]any)
				if !ok {
					continue
				}
				for name, prop := range props {
					childPath := append(append([]string(nil), path...), name)
					if err := walkSchema(prop, childPath, true, found); err != nil {
						return err
					}
				}
				continue
			}
			if err := walkSchema(child, path, false, found); err != nil {
				return err
			}
		}
	}
	return nil
}

func newParamHeader(rawName, rawType any, path []string) (paramHeader, error) {
	name, ok := rawName.(string)
	if !ok {
		return paramHeader{}, errors.New("x-mcp-header must be a string")
	}
	if name == "" {
		return paramHeader{}, errors.New("x-mcp-header must not be empty")
	}
	if !isToken(name) {
		return paramHeader{}, fmt.Errorf("x-mcp-header %q is not a valid HTTP field-name token", name)
	}
	jsonType, _ := rawType.(string)
	switch jsonType {
	case "string", "integer", "boolean":
	default:
		return paramHeader{}, fmt.Errorf("x-mcp-header %q is on a property of type %q; only string, integer, and boolean are allowed", name, jsonType)
	}
	return paramHeader{name: name, path: path, jsonType: jsonType}, nil
}

// isToken reports whether s is an RFC 9110 token (1*tchar).
func isToken(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return s != ""
}

// checkParamHeaders compares the Mcp-Param-* headers against the tool call's
// arguments. Every annotated argument with a value must have a matching
// header, and an annotated argument without a value (or null) must not.
// Headers with no annotation are ignored, as the spec requires.
func checkParamHeaders(h http.Header, schema, params json.RawMessage) error {
	annotations, err := paramHeaders(schema)
	if err != nil {
		return fmt.Errorf("tool definition: %w", err)
	}
	if len(annotations) == 0 {
		return nil
	}

	var call struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return errors.New("invalid tools/call params")
	}
	var args any
	if len(call.Arguments) > 0 {
		dec := json.NewDecoder(bytes.NewReader(call.Arguments))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return errors.New("invalid arguments")
		}
	}

	for _, a := range annotations {
		value, present := lookup(args, a.path)
		header, sent := h[http.CanonicalHeaderKey(paramHeaderPrefix+a.name)]
		switch {
		case !present && !sent:
			continue
		case !present:
			return fmt.Errorf("%s%s header sent but %s has no value", paramHeaderPrefix, a.name, strings.Join(a.path, "."))
		case !sent:
			return fmt.Errorf("%s%s header missing for %s", paramHeaderPrefix, a.name, strings.Join(a.path, "."))
		}
		decoded, err := decodeHeaderValue(header[0])
		if err != nil {
			return fmt.Errorf("%s%s header has invalid base64", paramHeaderPrefix, a.name)
		}
		if !headerMatches(decoded, value) {
			return fmt.Errorf("%s%s header does not match %s", paramHeaderPrefix, a.name, strings.Join(a.path, "."))
		}
	}
	return nil
}

// lookup returns the value at path, treating JSON null as no value.
func lookup(args any, path []string) (any, bool) {
	node := args
	for _, key := range path {
		obj, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = obj[key]; !ok {
			return nil, false
		}
	}
	return node, node != nil
}

// headerMatches compares a decoded header with an argument value the way
// the spec's value encoding writes it.
func headerMatches(header string, value any) bool {
	switch v := value.(type) {
	case string:
		return header == v
	case bool:
		return header == strconv.FormatBool(v)
	case json.Number:
		// The spec asks for a numeric comparison, so 42.0 matches 42.
		want, err1 := v.Float64()
		got, err2 := strconv.ParseFloat(header, 64)
		return err1 == nil && err2 == nil && got == want
	default:
		// Objects and arrays cannot be mirrored into a header.
		return false
	}
}
