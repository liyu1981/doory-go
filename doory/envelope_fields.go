// Ordered validation-error envelopes.
//
// DRF's api_exception_handler renders {"error":{"code":"validation_error",
// "message":"Invalid input.","fields":{...},"field_codes":{...}}} with
// fields in SERIALIZER DECLARATION ORDER (Python dicts preserve insertion;
// probes/0002-accounts show single-field cases and the field/code pairing).
// Go maps have no order — fields arrive as an ordered slice and the body is
// assembled by hand so bytes match. Compact encoding, no HTML escaping
// (DRF COMPACT_JSON + UNICODE_JSON, probed).
package doory

import (
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"
)

// FieldError is one field's messages and their Django codes, in order.
// Raw, when set, is the pre-rendered JSON value used verbatim — for
// NESTED DRF detail shapes like ListField child errors
// (probe0003: {"scopes":{"0":["\"nope:bad\" is not a valid choice."]}}).
type FieldError struct {
	Name     string
	Messages []string
	Codes    []string
	// Raw renders the fields value verbatim; RawCodes the field_codes
	// value (they differ for nested shapes: message text vs code).
	Raw      string
	RawCodes string
}

// SimpleField builds a flat field error (messages + codes in lockstep).
func SimpleField(name string, pairs ...string) FieldError {
	f := FieldError{Name: name}
	for i := 0; i+1 < len(pairs); i += 2 {
		f.Messages = append(f.Messages, pairs[i])
		f.Codes = append(f.Codes, pairs[i+1])
	}
	return f
}

// writeDRFValidation renders a validation_error envelope at 400.
func writeDRFValidation(w http.ResponseWriter, fields []FieldError) {
	b := assembleValidation(fields)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(b)
}

func assembleValidation(fields []FieldError) []byte {
	var b []byte
	b = append(b, `{"error":{"code":"validation_error","message":"Invalid input.","fields":{`...)
	for i, f := range fields {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, jsonName(f.Name)...)
		b = append(b, ':')
		if f.Raw != "" {
			b = append(b, f.Raw...)
		} else {
			b = append(b, '[')
			for j, m := range f.Messages {
				if j > 0 {
					b = append(b, ',')
				}
				b = append(b, jsonStr(m)...)
			}
			b = append(b, ']')
		}
	}
	b = append(b, `},"field_codes":{`...)
	for i, f := range fields {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, jsonName(f.Name)...)
		b = append(b, ':')
		if f.RawCodes != "" {
			b = append(b, f.RawCodes...)
		} else if f.Raw != "" {
			b = append(b, f.Raw...)
		} else {
			b = append(b, '[')
			for j, c := range f.Codes {
				if j > 0 {
					b = append(b, ',')
				}
				if c == "" {
					// Django-core validators carry code=None -> JSON null
					// (probe0006 step03: field_codes script [null]).
					b = append(b, "null"...)
				} else {
					b = append(b, jsonStr(c)...)
				}
			}
			b = append(b, ']')
		}
	}
	b = append(b, '}', '}', '}')
	return b
}

// jsonStr renders a JSON string like Python's json.dumps (compact, no HTML
// escaping — DRF UNICODE_JSON keeps non-ASCII raw too).
func jsonStr(s string) string {
	var b []byte
	b = append(b, '"')
	var buf [4]byte
	for _, r := range s {
		switch {
		case r == '"':
			b = append(b, '\\', '"')
		case r == '\\':
			b = append(b, '\\', '\\')
		case r == '\n':
			b = append(b, '\\', 'n')
		case r == '\r':
			b = append(b, '\\', 'r')
		case r == '\t':
			b = append(b, '\\', 't')
		case r < 0x20 || r == 0x7f:
			b = append(b, []byte(fmt.Sprintf("\\u%04x", r))...)
		case r < 0x80:
			b = append(b, byte(r))
		default:
			n := utf8.EncodeRune(buf[:], r)
			b = append(b, buf[:n]...)
		}
	}
	b = append(b, '"')
	return string(b)
}

func jsonName(s string) string { return jsonStr(s) }
