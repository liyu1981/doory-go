// Package protocol implements the Yjs binary wire format: LEB128 unsigned varints,
// variable-length byte arrays, and UTF-8 strings, matching lib0/encoding exactly.
package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// WriteVarUint writes a uint64 as a LEB128 unsigned varint (7-bit payload per byte,
// MSB set when more bytes follow). This matches lib0/encoding.writeVarUint.
func WriteVarUint(w io.Writer, v uint64) error {
	var buf [binary.MaxVarintLen64]byte
	n := 0
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		buf[n] = b
		n++
		if v == 0 {
			break
		}
	}
	_, err := w.Write(buf[:n])
	return err
}

// ReadVarUint reads a LEB128 unsigned varint from r.
func ReadVarUint(r io.Reader) (uint64, error) {
	var result uint64
	var shift uint
	buf := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return 0, fmt.Errorf("yjs/protocol: truncated varint")
			}
			return 0, err
		}
		b := buf[0]
		result |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, fmt.Errorf("yjs/protocol: varint overflow")
		}
	}
}

// WriteVarBytes writes a length-prefixed byte slice: varuint(len) + bytes.
func WriteVarBytes(w io.Writer, b []byte) error {
	if err := WriteVarUint(w, uint64(len(b))); err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	_, err := w.Write(b)
	return err
}

// ReadVarBytes reads a length-prefixed byte slice.
func ReadVarBytes(r io.Reader) ([]byte, error) {
	n, err := ReadVarUint(r)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("yjs/protocol: truncated bytes (want %d): %w", n, err)
	}
	return buf, nil
}

// WriteVarString writes a UTF-8 string as varuint(UTF-8 byte length) + UTF-8 bytes.
func WriteVarString(w io.Writer, s string) error {
	return WriteVarBytes(w, []byte(s))
}

// ReadVarString reads a length-prefixed UTF-8 string.
func ReadVarString(r io.Reader) (string, error) {
	b, err := ReadVarBytes(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
