package pcln

import (
	"bytes"
	"debug/elf"
	"os"
	"testing"
)

func TestRoundTripReferenceBinary(t *testing.T) {
	data := loadGopclntab(t, "../testdata/helper_linux_amd64")
	if data == nil {
		t.Skip("reference binary not present")
	}
	xformed, meta, err := Encode(data)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(xformed) == 0 {
		t.Fatal("Encode returned empty")
	}
	decoded, err := Decode(xformed, meta)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("pcln round-trip FAILED: len got=%d want=%d", len(decoded), len(data))
	}
}

func TestEncodeShrinksReferencePcln(t *testing.T) {
	data := loadGopclntab(t, "../testdata/helper_linux_amd64")
	if data == nil {
		t.Skip("reference binary not present")
	}
	xformed, _, err := Encode(data)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Our transform is close to length-preserving; ftab shrinks by ~25 KB.
	if len(xformed) >= len(data) {
		t.Errorf("expected transformed pcln to be smaller than raw: got %d vs %d",
			len(xformed), len(data))
	}
	t.Logf("raw pcln=%d xformed=%d  Δ=%d", len(data), len(xformed), len(data)-len(xformed))
}

func TestRejectUnsupportedMagic(t *testing.T) {
	data := make([]byte, 256)
	data[0] = 0xFA // wrong magic
	data[1] = 0xFF
	data[2] = 0xFF
	data[3] = 0xFF
	_, _, err := Encode(data)
	if err == nil {
		t.Fatal("expected error on unsupported magic")
	}
}

func loadGopclntab(t *testing.T, path string) []byte {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sec := f.Section(".gopclntab")
	if sec == nil {
		return nil
	}
	data, err := sec.Data()
	if err != nil {
		return nil
	}
	return data
}

func loadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}
