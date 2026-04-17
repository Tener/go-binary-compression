package pipeline

import (
	"bytes"
	"crypto/sha256"
	"os"
	"testing"
)

func TestRoundTripReferenceBinary(t *testing.T) {
	raw, err := os.ReadFile("../testdata/helper_linux_amd64")
	if err != nil {
		t.Skipf("reference binary not present: %v", err)
	}
	origSum := sha256.Sum256(raw)

	blob, err := Encode(raw)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(blob) >= len(raw)+256 {
		t.Errorf("encoded blob grew unexpectedly: %d vs %d", len(blob), len(raw))
	}

	restored, err := Decode(blob)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	decSum := sha256.Sum256(restored)
	if decSum != origSum {
		t.Fatalf("byte-level round-trip FAILED")
	}
	if !bytes.Equal(restored, raw) {
		t.Fatal("bytes.Equal disagrees with sha256 (shouldn't happen)")
	}
	t.Logf("encoded blob: %d bytes (raw=%d, Δ=%+d)", len(blob), len(raw), len(blob)-len(raw))
}

func TestEncodeRejectsNonELF(t *testing.T) {
	_, err := Encode([]byte("not an elf"))
	if err == nil {
		t.Fatal("expected error on non-ELF input")
	}
}

func BenchmarkEncodeDecodeReference(b *testing.B) {
	raw, err := os.ReadFile("../testdata/helper_linux_amd64")
	if err != nil {
		b.Skipf("reference binary not present: %v", err)
	}
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		blob, err := Encode(raw)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := Decode(blob); err != nil {
			b.Fatal(err)
		}
	}
}
