package pcln

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"strconv"
	"testing"
)

func TestDecodeToPctabBytes(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for _, size := range []int{0, 1, 2, 3, 100, 8193, 65537} {
		raw := make([]byte, size)
		random.Read(raw)
		val, pc := pctabParse(raw)
		encoded := append(bytes.Clone(val), pc...)
		encoded = append(encoded, 0, 0) // one entryoff and one funcoff
		m := Meta{
			ValLen: uint64(len(val)), PcLen: uint64(len(pc)),
			PctabSize: uint64(size), EDeltaLen: 1, FDeltaLen: 1, FtabSize: 8,
		}
		var out bytes.Buffer
		if err := DecodeTo(&out, bytes.NewReader(encoded), int64(len(encoded)), m); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		want := append(bytes.Clone(raw), make([]byte, 8)...)
		if !bytes.Equal(out.Bytes(), want) {
			t.Fatalf("size %d: pctab bytes differ", size)
		}
	}
}

func syntheticPcln(nfunc int) []byte {
	ftabSize := (nfunc + 1) * 8
	h := pcHeader{
		Magic: Magic118, PtrSize: 8, MinLC: 1, Nfunc: uint64(nfunc),
		FuncnameOffset: 72, CuOffset: 72, FiletabOffset: 72,
		PctabOffset: 72, PclnOffset: 72,
	}
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, h)
	ftab := make([]byte, ftabSize)
	records := make([]byte, 64*nfunc+7)
	for i := 0; i < nfunc; i++ {
		binary.LittleEndian.PutUint32(ftab[i*8:], uint32(i*16))
		binary.LittleEndian.PutUint32(ftab[i*8+4:], uint32(ftabSize+i*64))
		rec := records[i*64 : i*64+64]
		for c := 0; c < 10; c++ {
			binary.LittleEndian.PutUint32(rec[c*4:], uint32((i+c)%256))
		}
		binary.LittleEndian.PutUint32(rec[28:], 2)
		copy(rec[40:], []byte{1, 2, 3, 1})
		copy(rec[44:], []byte("pcdata..tail!padding."))
	}
	binary.LittleEndian.PutUint32(ftab[nfunc*8:], uint32(nfunc*16))
	out.Write(ftab)
	out.Write(records)
	return out.Bytes()
}

func TestDecodeToSyntheticRecords(t *testing.T) {
	for _, nfunc := range []int{0, 1, 1000} {
		raw := syntheticPcln(nfunc)
		encoded, meta, err := Encode(raw)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := DecodeTo(&out, bytes.NewReader(encoded), int64(len(encoded)), meta); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), raw) {
			t.Fatalf("nfunc %d: reconstructed records differ", nfunc)
		}
	}
}

func BenchmarkDecodeToFunctionCount(b *testing.B) {
	for _, nfunc := range []int{1000, 100000} {
		encoded, meta, err := Encode(syntheticPcln(nfunc))
		if err != nil {
			b.Fatal(err)
		}
		b.Run(strconv.Itoa(nfunc), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := DecodeTo(io.Discard, bytes.NewReader(encoded), int64(len(encoded)), meta); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
