package protocol

import (
	"bytes"
	"math"
	"testing"
)

func TestVarintRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 63, 64, 127, 128, 255, 256, 16383, 16384, math.MaxUint32, math.MaxUint64}
	for _, v := range cases {
		var buf bytes.Buffer
		if err := WriteVarUint(&buf, v); err != nil {
			t.Fatalf("WriteVarUint(%d): %v", v, err)
		}
		got, err := ReadVarUint(&buf)
		if err != nil {
			t.Fatalf("ReadVarUint(%d): %v", v, err)
		}
		if got != v {
			t.Errorf("round-trip %d: got %d", v, got)
		}
	}
}

func TestVarintWireFormat(t *testing.T) {
	// Verify exact bytes match the lib0/encoding LEB128 spec.
	// 0 -> [0x00]
	// 1 -> [0x01]
	// 127 -> [0x7f]
	// 128 -> [0x80, 0x01]
	// 300 -> [0xac, 0x02]
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
		{16383, []byte{0xff, 0x7f}},
		{16384, []byte{0x80, 0x80, 0x01}},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		if err := WriteVarUint(&buf, tc.v); err != nil {
			t.Fatalf("WriteVarUint(%d): %v", tc.v, err)
		}
		got := buf.Bytes()
		if !bytes.Equal(got, tc.want) {
			t.Errorf("v=%d: want %v got %v", tc.v, tc.want, got)
		}
	}
}

func TestVarStringRoundTrip(t *testing.T) {
	cases := []string{"", "hello", "world!", "unicode: \u00e9\u00e0\u4e2d\u6587", string(make([]byte, 300))}
	for _, s := range cases {
		var buf bytes.Buffer
		if err := WriteVarString(&buf, s); err != nil {
			t.Fatalf("WriteVarString(%q): %v", s, err)
		}
		got, err := ReadVarString(&buf)
		if err != nil {
			t.Fatalf("ReadVarString(%q): %v", s, err)
		}
		if got != s {
			t.Errorf("round-trip %q: got %q", s, got)
		}
	}
}

func TestVarBytesRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0x00},
		{0x01, 0x02, 0x03},
		make([]byte, 300),
	}
	for _, b := range cases {
		var buf bytes.Buffer
		if err := WriteVarBytes(&buf, b); err != nil {
			t.Fatalf("WriteVarBytes: %v", err)
		}
		got, err := ReadVarBytes(&buf)
		if err != nil {
			t.Fatalf("ReadVarBytes: %v", err)
		}
		if !bytes.Equal(got, b) {
			t.Errorf("round-trip bytes: got %v want %v", got, b)
		}
	}
}

func TestReadVarUintEOF(t *testing.T) {
	var buf bytes.Buffer
	_, err := ReadVarUint(&buf)
	if err == nil {
		t.Error("expected error on empty buffer")
	}
}

func TestReadVarUintTruncated(t *testing.T) {
	// Write a multi-byte varint but only give half of it.
	buf := bytes.NewBuffer([]byte{0x80}) // continuation bit set, no next byte
	_, err := ReadVarUint(buf)
	if err == nil {
		t.Error("expected error on truncated varint")
	}
}
