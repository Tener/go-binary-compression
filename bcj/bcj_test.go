package bcj

import (
	"bytes"
	"crypto/rand"
	"math"
	mrand "math/rand"
	"os"
	"testing"
)

func TestRoundTripEmpty(t *testing.T) {
	var buf []byte
	Encode(buf)
	Decode(buf)
}

func TestRoundTripShort(t *testing.T) {
	// too short to contain an E8/E9 + 4 bytes
	buf := []byte{0xE8, 0x00, 0x00}
	orig := append([]byte{}, buf...)
	Encode(buf)
	Decode(buf)
	if !bytes.Equal(buf, orig) {
		t.Fatalf("mutated short buffer: got %x want %x", buf, orig)
	}
}

func TestRoundTripSynthetic(t *testing.T) {
	// Every pattern of E8/E9 followed by disp32. Round-trip must be stable.
	cases := [][]byte{
		{0xE8, 0x00, 0x00, 0x00, 0x00},
		{0xE9, 0xFB, 0xFF, 0xFF, 0xFF},
		{0xE8, 0xDE, 0xAD, 0xBE, 0xEF, 0xE9, 0x12, 0x34, 0x56, 0x78},
		{0xCC, 0xE8, 0x00, 0x00, 0x00, 0x00, 0xCC, 0xCC},
		// back-to-back E8s
		{0xE8, 0x00, 0x00, 0x00, 0x00, 0xE8, 0x00, 0x00, 0x00, 0x00},
	}
	for _, c := range cases {
		orig := append([]byte{}, c...)
		Encode(c)
		Decode(c)
		if !bytes.Equal(c, orig) {
			t.Fatalf("round-trip failed: got %x want %x", c, orig)
		}
	}
}

func TestRoundTripRandom(t *testing.T) {
	r := mrand.New(mrand.NewSource(1))
	for trial := 0; trial < 50; trial++ {
		size := r.Intn(65536) + 5
		buf := make([]byte, size)
		_, _ = rand.Read(buf)
		orig := append([]byte{}, buf...)
		Encode(buf)
		Decode(buf)
		if !bytes.Equal(buf, orig) {
			t.Fatalf("random round-trip failed trial=%d size=%d", trial, size)
		}
	}
}

func TestRoundTripReferenceBinary(t *testing.T) {
	buf, err := os.ReadFile("../testdata/helper_linux_amd64")
	if err != nil {
		t.Skipf("reference binary not present: %v", err)
	}
	orig := append([]byte{}, buf...)
	Encode(buf)
	if bytes.Equal(buf, orig) {
		t.Fatalf("Encode did nothing on a real Go binary — broken?")
	}
	Decode(buf)
	if !bytes.Equal(buf, orig) {
		t.Fatal("reference binary round-trip FAILED")
	}
}

func TestEncodeChangesBytes(t *testing.T) {
	// A CALL at offset 0x100 with disp=0 must become abs=0x105 after encode.
	buf := make([]byte, 0x200)
	buf[0x100] = 0xE8
	// disp = 0 already (zero-init)
	Encode(buf)
	got := uint32(buf[0x101]) | uint32(buf[0x102])<<8 | uint32(buf[0x103])<<16 | uint32(buf[0x104])<<24
	if got != 0x105 {
		t.Fatalf("want abs=0x105, got %#x", got)
	}
}

func BenchmarkEncode1MB(b *testing.B) {
	buf := make([]byte, 1<<20)
	_, _ = rand.Read(buf)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Encode(buf)
	}
}

func BenchmarkEncodeReferenceBinary(b *testing.B) {
	buf, err := os.ReadFile("../testdata/helper_linux_amd64")
	if err != nil {
		b.Skipf("reference binary not present: %v", err)
	}
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Encode(buf)
		Decode(buf)
	}
	_ = math.Abs(0) // keep import used in future math-related benches
}
