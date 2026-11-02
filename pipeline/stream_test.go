package pipeline

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/Tener/go-binary-compression/bcj"
	"github.com/Tener/go-binary-compression/pcln"
)

func TestDecodeToSectionOrders(t *testing.T) {
	raw, err := os.ReadFile("../testdata/helper_linux_amd64")
	if err != nil {
		t.Fatal(err)
	}
	f, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	ps, ts := f.Section(".gopclntab"), f.Section(".text")
	pclnRaw := raw[ps.Offset : ps.Offset+ps.Size]
	textRaw := raw[ts.Offset : ts.Offset+ts.Size]
	xformed, meta, err := pcln.Encode(pclnRaw)
	if err != nil {
		t.Fatal(err)
	}
	text := bytes.Clone(textRaw)
	bcj.Encode(text)
	for _, pclnFirst := range []bool{false, true} {
		env := Envelope{TextSize: ts.Size, PclnSize: ps.Size, XformLen: uint64(len(xformed)), PclnMeta: meta}
		var blob, want bytes.Buffer
		prefix, gap, suffix := []byte("prefix"), []byte("gap"), []byte("suffix")
		if pclnFirst {
			env.PclnOff = uint64(len(prefix))
			env.TextOff = env.PclnOff + env.PclnSize + uint64(len(gap))
		} else {
			env.TextOff = uint64(len(prefix))
			env.PclnOff = env.TextOff + env.TextSize + uint64(len(gap))
		}
		if err := binary.Write(&blob, binary.LittleEndian, env); err != nil {
			t.Fatal(err)
		}
		blob.Write(prefix)
		want.Write(prefix)
		if pclnFirst {
			blob.Write(xformed)
			blob.Write(gap)
			blob.Write(text)
			want.Write(pclnRaw)
			want.Write(gap)
			want.Write(textRaw)
		} else {
			blob.Write(text)
			blob.Write(gap)
			blob.Write(xformed)
			want.Write(textRaw)
			want.Write(gap)
			want.Write(pclnRaw)
		}
		blob.Write(suffix)
		want.Write(suffix)
		var got bytes.Buffer
		if err := DecodeTo(&got, bytes.NewReader(blob.Bytes()), int64(blob.Len())); err != nil {
			t.Fatalf("pclnFirst=%t: %v", pclnFirst, err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("pclnFirst=%t: decoded bytes differ", pclnFirst)
		}
	}
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestDecodeToErrors(t *testing.T) {
	raw, err := os.ReadFile("../testdata/helper_linux_amd64")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := Encode(raw)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("write failed")
	if err := DecodeTo(failingWriter{writeErr}, bytes.NewReader(blob), int64(len(blob))); !errors.Is(err, writeErr) {
		t.Fatalf("writer error: got %v", err)
	}
	for _, size := range []int{0, 1, (Envelope{}).Size() - 1, len(blob) / 2} {
		if err := DecodeTo(io.Discard, bytes.NewReader(blob[:size]), int64(size)); err == nil {
			t.Fatalf("size %d: expected error", size)
		}
	}
	var original Envelope
	if err := binary.Read(bytes.NewReader(blob), binary.LittleEndian, &original); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Envelope){
		func(e *Envelope) { e.TextOff = math.MaxUint64 },
		func(e *Envelope) { e.TextSize = math.MaxUint64 },
		func(e *Envelope) { e.XformLen = math.MaxUint64 },
		func(e *Envelope) { e.PclnSize = math.MaxUint64 },
		func(e *Envelope) { e.PclnOff = e.TextOff },
		func(e *Envelope) { e.PclnMeta.Nfunc = math.MaxUint64 },
		func(e *Envelope) { e.PclnMeta.NameOffDLen = math.MaxUint64 },
	} {
		env := original
		mutate(&env)
		var bad bytes.Buffer
		if err := binary.Write(&bad, binary.LittleEndian, env); err != nil {
			t.Fatal(err)
		}
		bad.Write(blob[env.Size():])
		if err := DecodeTo(io.Discard, bytes.NewReader(bad.Bytes()), int64(bad.Len())); err == nil {
			t.Fatal("expected invalid envelope error")
		}
	}
}

func BenchmarkDecodeReference(b *testing.B) {
	raw, err := os.ReadFile("../testdata/helper_linux_amd64")
	if err != nil {
		b.Fatal(err)
	}
	blob, err := Encode(raw)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Decode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(raw)))
		for i := 0; i < b.N; i++ {
			if _, err := Decode(blob); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, copies := range []int{1, 16} {
		// Append passthrough data to vary the file size without changing
		// the section layout or allocating it inside the measured loop.
		input := make([]byte, len(blob)+(copies-1)*len(raw))
		copy(input, blob)
		b.Run("DecodeTo/"+strconv.Itoa(copies)+"x", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw) * copies))
			for i := 0; i < b.N; i++ {
				if err := DecodeTo(io.Discard, bytes.NewReader(input), int64(len(input))); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
