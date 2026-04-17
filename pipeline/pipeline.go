// Package pipeline wires together BCJ (on the .text section) and the pcln
// transform (on .gopclntab) into a single ELF-aware encoder/decoder.
//
// The output is a self-describing byte blob: a fixed-size envelope header
// carrying the original file offsets and pcln metadata, followed by the
// transformed body. Decode reads the envelope, reverses each transform in
// place, and returns bytes identical to the input.
package pipeline

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"

	"github.com/Tener/go-binary-compression/bcj"
	"github.com/Tener/go-binary-compression/pcln"
)

// Envelope is the fixed-size prefix of every encoded blob. It is stored in
// little-endian binary form (see Envelope{}.Size()).
type Envelope struct {
	TextOff   uint64
	TextSize  uint64
	PclnOff   uint64
	PclnSize  uint64
	XformLen  uint64
	PclnMeta  pcln.Meta
}

// Size returns the on-wire byte length of the Envelope struct.
func (Envelope) Size() int { return binary.Size(Envelope{}) }

// Encode reads an amd64 Linux ELF from rawELF and produces a transformed
// byte blob that compresses better than the input for all tested algorithms.
// The input must have .text, .gopclntab, and .text must come after
// .gopclntab in file order (the common Go linker layout).
func Encode(rawELF []byte) ([]byte, error) {
	f, err := elf.NewFile(bytes.NewReader(rawELF))
	if err != nil {
		return nil, fmt.Errorf("parse ELF: %w", err)
	}
	ts := f.Section(".text")
	ps := f.Section(".gopclntab")
	if ts == nil {
		return nil, fmt.Errorf("missing .text section")
	}
	if ps == nil {
		return nil, fmt.Errorf("missing .gopclntab section")
	}
	if ps.Offset >= ts.Offset {
		return nil, fmt.Errorf("unsupported layout: .gopclntab must precede .text in file order")
	}

	pclnBytes := make([]byte, ps.Size)
	copy(pclnBytes, rawELF[ps.Offset:ps.Offset+ps.Size])
	xformedPcln, meta, err := pcln.Encode(pclnBytes)
	if err != nil {
		return nil, fmt.Errorf("pcln encode: %w", err)
	}

	env := Envelope{
		TextOff: ts.Offset, TextSize: ts.Size,
		PclnOff: ps.Offset, PclnSize: ps.Size,
		XformLen: uint64(len(xformedPcln)),
		PclnMeta: meta,
	}

	var out bytes.Buffer
	if err := binary.Write(&out, binary.LittleEndian, env); err != nil {
		return nil, err
	}

	// [0 .. pcln)
	out.Write(rawELF[:ps.Offset])
	// transformed pcln
	out.Write(xformedPcln)
	// [pcln_end .. text)
	out.Write(rawELF[ps.Offset+ps.Size : ts.Offset])
	// BCJ(text)
	text := make([]byte, ts.Size)
	copy(text, rawELF[ts.Offset:ts.Offset+ts.Size])
	bcj.Encode(text)
	out.Write(text)
	// [text_end .. EOF)
	out.Write(rawELF[ts.Offset+ts.Size:])
	return out.Bytes(), nil
}

// Decode reverses Encode, returning bytes identical to the original ELF.
func Decode(blob []byte) ([]byte, error) {
	var env Envelope
	hdrSize := env.Size()
	if len(blob) < hdrSize {
		return nil, fmt.Errorf("blob too short for envelope (need %d, got %d)", hdrSize, len(blob))
	}
	if err := binary.Read(bytes.NewReader(blob[:hdrSize]), binary.LittleEndian, &env); err != nil {
		return nil, fmt.Errorf("read envelope: %w", err)
	}
	body := blob[hdrSize:]

	var out bytes.Buffer
	out.Write(body[:env.PclnOff])

	xformedPcln := body[env.PclnOff : env.PclnOff+env.XformLen]
	restored, err := pcln.Decode(xformedPcln, env.PclnMeta)
	if err != nil {
		return nil, fmt.Errorf("pcln decode: %w", err)
	}
	if uint64(len(restored)) != env.PclnSize {
		return nil, fmt.Errorf("pcln decode size mismatch: got %d want %d", len(restored), env.PclnSize)
	}
	out.Write(restored)

	gapStart := env.PclnOff + env.XformLen
	gapEnd := gapStart + (env.TextOff - (env.PclnOff + env.PclnSize))
	out.Write(body[gapStart:gapEnd])

	text := make([]byte, env.TextSize)
	copy(text, body[gapEnd:gapEnd+env.TextSize])
	bcj.Decode(text)
	out.Write(text)

	out.Write(body[gapEnd+env.TextSize:])
	return out.Bytes(), nil
}
