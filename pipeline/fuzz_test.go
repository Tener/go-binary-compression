package pipeline

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/Tener/go-binary-compression/bcj"
	"github.com/Tener/go-binary-compression/pcln"
)

type fuzzWriter struct {
	n uint64
}

func (w *fuzzWriter) Write(p []byte) (int, error) {
	w.n += uint64(len(p))
	return len(p), nil
}

func FuzzDecodeTo(f *testing.F) {
	// Two records with pcdata, funcdata, gaps, and trailing bytes.
	pctab := []byte{0, 1, 2, 0, 0x81, 1, 0x80, 1}
	rawPcln := make([]byte, 72+len(pctab)+24+128+7)
	binary.LittleEndian.PutUint32(rawPcln, pcln.Magic118)
	rawPcln[6], rawPcln[7] = 1, 8
	binary.LittleEndian.PutUint64(rawPcln[8:], 2)
	for off := 32; off <= 56; off += 8 {
		binary.LittleEndian.PutUint64(rawPcln[off:], 72)
	}
	binary.LittleEndian.PutUint64(rawPcln[64:], uint64(72+len(pctab)))
	copy(rawPcln[72:], pctab)
	ftab := rawPcln[72+len(pctab):]
	for i := 0; i < 2; i++ {
		binary.LittleEndian.PutUint32(ftab[i*8:], uint32(i*16))
		binary.LittleEndian.PutUint32(ftab[i*8+4:], uint32(24+i*64))
		rec := ftab[24+i*64:]
		binary.LittleEndian.PutUint32(rec, uint32(i*16))
		binary.LittleEndian.PutUint32(rec[28:], 2)
		rec[43] = 1
		copy(rec[44:64], []byte("pcdata..tail!padding."))
	}
	binary.LittleEndian.PutUint32(ftab[16:], 32)
	xformed, meta, err := pcln.Encode(rawPcln)
	if err != nil {
		f.Fatal(err)
	}
	for _, pclnFirst := range []bool{false, true} {
		for _, textSize := range []int{17, 64*1024 + 5} {
			rawText := bytes.Repeat([]byte{0xE8, 0xE9, 0, 0x80, 0xFF}, (textSize+4)/5)[:textSize]
			text := bytes.Clone(rawText)
			bcj.Encode(text)
			env := Envelope{
				TextSize: uint64(len(text)), PclnSize: uint64(len(rawPcln)),
				XformLen: uint64(len(xformed)), PclnMeta: meta,
			}
			var blob, want bytes.Buffer
			if pclnFirst {
				env.PclnOff = 6
				env.TextOff = 6 + env.PclnSize + 3
			} else {
				env.TextOff = 6
				env.PclnOff = 6 + env.TextSize + 3
			}
			if err := binary.Write(&blob, binary.LittleEndian, env); err != nil {
				f.Fatal(err)
			}
			blob.WriteString("prefix")
			want.WriteString("prefix")
			if pclnFirst {
				blob.Write(xformed)
				blob.WriteString("gap")
				blob.Write(text)
				want.Write(rawPcln)
				want.WriteString("gap")
				want.Write(rawText)
			} else {
				blob.Write(text)
				blob.WriteString("gap")
				blob.Write(xformed)
				want.Write(rawText)
				want.WriteString("gap")
				want.Write(rawPcln)
			}
			blob.WriteString("suffix")
			want.WriteString("suffix")
			var decoded bytes.Buffer
			if err := DecodeTo(&decoded, bytes.NewReader(blob.Bytes()), int64(blob.Len())); err != nil {
				f.Fatal(err)
			}
			if !bytes.Equal(decoded.Bytes(), want.Bytes()) {
				f.Fatal("seed does not round-trip")
			}
			f.Add(blob.Bytes(), int64(blob.Len()))
			if textSize == 17 {
				// Preserve the envelope while exercising truncated sources
				// and sizes that disagree with the underlying reader.
				for _, cut := range []int{env.Size() - 1, env.Size(), blob.Len() - 1} {
					f.Add(blob.Bytes()[:cut], int64(cut))
					f.Add(blob.Bytes()[:cut], int64(blob.Len()))
				}
				f.Add(blob.Bytes(), int64(-1))
				f.Add(blob.Bytes(), int64(math.MaxInt64))
			}
		}
	}
	f.Add([]byte{}, int64(0))
	f.Add(bytes.Repeat([]byte{0xFF}, (Envelope{}).Size()), int64((Envelope{}).Size()))
	f.Fuzz(func(t *testing.T, blob []byte, size int64) {
		var out fuzzWriter
		if err := DecodeTo(&out, bytes.NewReader(blob), size); err != nil {
			return
		}
		var env Envelope
		if err := binary.Read(bytes.NewReader(blob), binary.LittleEndian, &env); err != nil {
			t.Fatal(err)
		}
		want := uint64(size-int64(env.Size())) - env.XformLen + env.PclnSize
		if out.n != want {
			t.Fatalf("decoded size %d, want %d", out.n, want)
		}
		if size > int64(len(blob)) {
			t.Fatal("accepted a truncated source")
		}
	})
}
