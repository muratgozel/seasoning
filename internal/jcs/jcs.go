// Package jcs produces RFC 8785 (JSON Canonicalization Scheme) output for the
// JSON subset seasoning signs: objects, arrays, strings, booleans, null and
// integers within ±2^53. Other numbers are rejected rather than formatted,
// because signatures must never depend on float formatting.
package jcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

const maxSafeInt = 1<<53 - 1

// Marshal encodes v with encoding/json, then canonicalizes the result.
func Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(b)
}

// Canonicalize re-encodes one JSON document canonically.
func Canonicalize(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("jcs: %w", err)
	}
	if dec.More() {
		return nil, errors.New("jcs: trailing data after the JSON value")
	}
	var buf bytes.Buffer
	if err := encode(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Value canonicalizes an already decoded value (decoded with UseNumber).
func Value(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encode(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		return encodeString(buf, x)
	case json.Number:
		return encodeNumber(buf, x)
	case float64:
		return encodeNumber(buf, json.Number(strconv.FormatFloat(x, 'f', -1, 64)))
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encode(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// RFC 8785 §3.2.3: sort by UTF-16 code units, not bytes.
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := encode(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jcs: unsupported type %T", v)
	}
	return nil
}

func encodeNumber(buf *bytes.Buffer, n json.Number) error {
	if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		if i > maxSafeInt || i < -maxSafeInt {
			return fmt.Errorf("jcs: integer %s is outside ±2^53", n)
		}
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("jcs: invalid number %s", n)
	}
	if f != math.Trunc(f) || math.Abs(f) > maxSafeInt {
		return fmt.Errorf("jcs: number %s is not an integer within ±2^53", n)
	}
	buf.WriteString(strconv.FormatInt(int64(f), 10))
	return nil
}

// encodeString follows ECMAScript JSON.stringify, which RFC 8785 adopts:
// short escapes for \b \t \n \f \r, \u00xx for other controls, and every
// other character as literal UTF-8.
func encodeString(buf *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("jcs: string is not valid UTF-8")
	}
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
	return nil
}

func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}
