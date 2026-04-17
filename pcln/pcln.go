// Package pcln implements a structure-aware transform for Go's .gopclntab
// section (the PC→line/file/function metadata table, sized at ~33% of a
// typical stripped Go binary). The transform separates the two sub-tables
// that compress poorly on their own:
//
//   - pctab: concatenated PC delta tables encoded as packed varints. The
//     byte-level varint layout defeats LZ matchers; this transform splits
//     each (val_delta, pc_delta) pair so the two streams can be compressed
//     together but matched against themselves.
//   - ftab: fixed-size (entryoff, funcoff) records. Both columns are
//     monotonically increasing, so delta-encoding each column as varints
//     collapses them by >90%.
//
// Header, funcname table, cutab, filetab, and the per-function records are
// passed through unchanged; they already compress close to entropy.
//
// The transform only runs on the 1.18+ pclntab magic (0xFFFFFFF1). For any
// other magic the input is returned unchanged with Meta.HeaderSize == 0 so
// callers can detect and store the raw bytes.
package pcln

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Magic118 is the pclntab magic introduced in Go 1.18.
const Magic118 = 0xFFFFFFF1

// Meta carries the minimal information the decoder needs to invert the
// transform. Callers should store it alongside (or concatenated with) the
// transformed byte stream.
type Meta struct {
	Nfunc       uint64
	ValLen      uint64
	PcLen       uint64
	EDeltaLen   uint64
	FDeltaLen   uint64
	PctabSize   uint64
	FtabSize    uint64
	FuncRecSize uint64
	HeaderSize  uint64 // original pcln offset up to start of pctab
}

type pcHeader struct {
	Magic                                                                                           uint32
	Pad1, Pad2, MinLC, PtrSize                                                                      uint8
	Nfunc, Nfiles, TextStart, FuncnameOffset, CuOffset, FiletabOffset, PctabOffset, PclnOffset uint64
}

// Encode applies the transform to a complete .gopclntab section. The
// returned Meta must be preserved (e.g., marshaled alongside the stream)
// to make Decode possible.
func Encode(data []byte) ([]byte, Meta, error) {
	h, err := readHeader(data)
	if err != nil {
		return nil, Meta{}, err
	}
	if h.Magic != Magic118 {
		return nil, Meta{}, fmt.Errorf("unsupported pclntab magic %#x (want %#x for Go 1.18+)", h.Magic, Magic118)
	}
	pctab := data[h.PctabOffset:h.PclnOffset]
	pcln := data[h.PclnOffset:]
	nfunc := int(h.Nfunc)
	ftabSize := (nfunc + 1) * 8
	if len(pcln) < ftabSize {
		return nil, Meta{}, errors.New("pclnTab shorter than ftab")
	}
	ftab := pcln[:ftabSize]
	funcRecs := pcln[ftabSize:]

	val, pc := pctabParse(pctab)

	eoff := make([]byte, 4*(nfunc+1))
	foff := make([]byte, 4*(nfunc+1))
	for i := 0; i <= nfunc; i++ {
		copy(eoff[i*4:], ftab[i*8:i*8+4])
		copy(foff[i*4:], ftab[i*8+4:i*8+8])
	}
	edelta := deltaVarint(eoff)
	fdelta := deltaVarint(foff)

	var out bytes.Buffer
	out.Write(data[:h.PctabOffset])
	out.Write(val)
	out.Write(pc)
	out.Write(edelta)
	out.Write(fdelta)
	out.Write(funcRecs)

	meta := Meta{
		Nfunc:       uint64(nfunc),
		ValLen:      uint64(len(val)),
		PcLen:       uint64(len(pc)),
		EDeltaLen:   uint64(len(edelta)),
		FDeltaLen:   uint64(len(fdelta)),
		PctabSize:   uint64(len(pctab)),
		FtabSize:    uint64(ftabSize),
		FuncRecSize: uint64(len(funcRecs)),
		HeaderSize:  h.PctabOffset,
	}
	return out.Bytes(), meta, nil
}

// Decode reverses Encode, reconstructing the original .gopclntab bytes.
func Decode(xformed []byte, m Meta) ([]byte, error) {
	off := m.HeaderSize
	if uint64(len(xformed)) < off+m.ValLen+m.PcLen+m.EDeltaLen+m.FDeltaLen+m.FuncRecSize {
		return nil, errors.New("transformed pcln too short for meta")
	}
	val := xformed[off : off+m.ValLen]
	off += m.ValLen
	pc := xformed[off : off+m.PcLen]
	off += m.PcLen
	ed := xformed[off : off+m.EDeltaLen]
	off += m.EDeltaLen
	fd := xformed[off : off+m.FDeltaLen]
	off += m.FDeltaLen
	funcRecs := xformed[off:]

	pctab := pctabInterleave(val, pc)
	if uint64(len(pctab)) != m.PctabSize {
		return nil, fmt.Errorf("pctab reconstruction size mismatch: got %d want %d", len(pctab), m.PctabSize)
	}

	eoff, err := undeltaVarint(ed, int(m.Nfunc)+1)
	if err != nil {
		return nil, fmt.Errorf("decode entryoff deltas: %w", err)
	}
	foff, err := undeltaVarint(fd, int(m.Nfunc)+1)
	if err != nil {
		return nil, fmt.Errorf("decode funcoff deltas: %w", err)
	}
	ftab := make([]byte, m.FtabSize)
	for i := 0; i <= int(m.Nfunc); i++ {
		copy(ftab[i*8:], eoff[i*4:i*4+4])
		copy(ftab[i*8+4:], foff[i*4:i*4+4])
	}

	var out bytes.Buffer
	out.Write(xformed[:m.HeaderSize])
	out.Write(pctab)
	out.Write(ftab)
	out.Write(funcRecs)
	return out.Bytes(), nil
}

func readHeader(data []byte) (pcHeader, error) {
	var h pcHeader
	if len(data) < 72 {
		return h, errors.New("pclntab too short for header")
	}
	return h, binary.Read(bytes.NewReader(data), binary.LittleEndian, &h)
}

// pctabParse walks pctab and splits it into two byte streams: the val-delta
// varint bytes, and the pc-delta varint bytes. A state machine alternates
// between the two. A 0 byte in the val state is a table terminator (single
// byte, no pc-delta follows it).
func pctabParse(b []byte) (val, pc []byte) {
	state := 0
	for i := 0; i < len(b); {
		if state == 0 {
			if b[i] == 0 {
				val = append(val, 0)
				i++
				continue
			}
			start := i
			for i < len(b) && b[i]&0x80 != 0 {
				i++
			}
			if i < len(b) {
				i++
			}
			val = append(val, b[start:i]...)
			state = 1
		} else {
			start := i
			for i < len(b) && b[i]&0x80 != 0 {
				i++
			}
			if i < len(b) {
				i++
			}
			pc = append(pc, b[start:i]...)
			state = 0
		}
	}
	return
}

func pctabInterleave(val, pc []byte) []byte {
	out := make([]byte, 0, len(val)+len(pc))
	vi, pi := 0, 0
	state := 0
	for vi < len(val) || pi < len(pc) {
		if state == 0 {
			if vi >= len(val) {
				break
			}
			if val[vi] == 0 {
				out = append(out, 0)
				vi++
				continue
			}
			start := vi
			for vi < len(val) && val[vi]&0x80 != 0 {
				vi++
			}
			if vi < len(val) {
				vi++
			}
			out = append(out, val[start:vi]...)
			state = 1
		} else {
			if pi >= len(pc) {
				break
			}
			start := pi
			for pi < len(pc) && pc[pi]&0x80 != 0 {
				pi++
			}
			if pi < len(pc) {
				pi++
			}
			out = append(out, pc[start:pi]...)
			state = 0
		}
	}
	return out
}

func deltaVarint(b []byte) []byte {
	out := make([]byte, 0, len(b))
	var prev uint32
	var tmp [binary.MaxVarintLen32]byte
	for i := 0; i+3 < len(b); i += 4 {
		v := binary.LittleEndian.Uint32(b[i:])
		d := v - prev
		prev = v
		n := binary.PutUvarint(tmp[:], uint64(d))
		out = append(out, tmp[:n]...)
	}
	return out
}

func undeltaVarint(b []byte, n int) ([]byte, error) {
	out := make([]byte, 4*n)
	var prev uint32
	i := 0
	for k := 0; k < n; k++ {
		v, c := binary.Uvarint(b[i:])
		if c <= 0 {
			return nil, fmt.Errorf("uvarint decode failure at element %d", k)
		}
		i += c
		val := prev + uint32(v)
		binary.LittleEndian.PutUint32(out[k*4:], val)
		prev = val
	}
	return out, nil
}
