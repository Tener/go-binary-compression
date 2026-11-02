package bcj

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

func TestDecodeTo(t *testing.T) {
	for _, size := range []int{0, 1, 4, 5, 65535, 65540, 65541, 200000} {
		raw := make([]byte, size)
		rand.New(rand.NewSource(int64(size))).Read(raw)
		for i := 65530; i+5 <= len(raw) && i < 65550; i += 5 {
			copy(raw[i:], []byte{0xE8, 0xE9, 0xE8, 0xE9, 0x80})
		}
		encoded := bytes.Clone(raw)
		Encode(encoded)
		var out bytes.Buffer
		if err := DecodeTo(&out, bytes.NewReader(encoded), int64(size)); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(out.Bytes(), raw) {
			t.Fatalf("size %d: decoded bytes differ", size)
		}
	}
}

func TestDecodeToTruncated(t *testing.T) {
	if err := DecodeTo(io.Discard, bytes.NewReader([]byte{0xE8}), 5); err == nil {
		t.Fatal("expected truncated input error")
	}
}
