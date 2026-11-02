package pcln

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

type fuzzWriter struct {
	n uint64
}

func (w *fuzzWriter) Write(p []byte) (int, error) {
	w.n += uint64(len(p))
	return len(p), nil
}

func FuzzDecodeTo(f *testing.F) {
	addSeed := func(data []byte, m Meta, original []byte) {
		var meta, decoded bytes.Buffer
		if err := binary.Write(&meta, binary.LittleEndian, m); err != nil {
			f.Fatal(err)
		}
		if err := DecodeTo(&decoded, bytes.NewReader(data), int64(len(data)), m); err != nil {
			f.Fatal(err)
		}
		if !bytes.Equal(decoded.Bytes(), original) {
			f.Fatal("seed does not round-trip")
		}
		f.Add(meta.Bytes(), data, int64(len(data)))
		for _, cut := range []int{0, len(data) / 2, len(data) - 1} {
			f.Add(meta.Bytes(), data[:cut], int64(cut))
			f.Add(meta.Bytes(), data[:cut], int64(len(data)))
		}
		f.Add(meta.Bytes(), data, int64(-1))
		f.Add(meta.Bytes(), data, int64(math.MaxInt64))
	}
	for _, nfunc := range []int{0, 1, 3} {
		raw := syntheticPcln(nfunc)
		encoded, meta, err := Encode(raw)
		if err != nil {
			f.Fatal(err)
		}
		addSeed(encoded, meta, raw)
	}
	for _, pctab := range [][]byte{
		{0},
		{1, 2, 0, 0x81, 1, 0x80, 1},
		{0x80}, // incomplete terminal varint
		{1, 0x80},
	} {
		val, pc := pctabParse(pctab)
		data := append(bytes.Clone(val), pc...)
		data = append(data, 0, 0)
		m := Meta{
			ValLen: uint64(len(val)), PcLen: uint64(len(pc)), PctabSize: uint64(len(pctab)),
			EDeltaLen: 1, FDeltaLen: 1, FtabSize: 8,
		}
		addSeed(data, m, append(bytes.Clone(pctab), make([]byte, 8)...))
	}
	f.Add(bytes.Repeat([]byte{0xFF}, binary.Size(Meta{})), []byte{}, int64(0))
	f.Fuzz(func(t *testing.T, meta, data []byte, size int64) {
		var m Meta
		if err := binary.Read(bytes.NewReader(meta), binary.LittleEndian, &m); err != nil {
			t.Skip()
		}
		var out fuzzWriter
		if err := DecodeTo(&out, bytes.NewReader(data), size, m); err != nil {
			return
		}
		want := m.HeaderSize + m.PctabSize + m.FtabSize + m.FuncRecSize
		if out.n != want {
			t.Fatalf("decoded size %d, want %d", out.n, want)
		}
		if size > int64(len(data)) {
			t.Fatal("accepted a truncated source")
		}
	})
}
