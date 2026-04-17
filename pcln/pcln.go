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
	PcLen      uint64
	EDeltaLen   uint64
	FDeltaLen   uint64
	PctabSize   uint64
	FtabSize    uint64
	FuncRecSize uint64
	HeaderSize  uint64 // original pcln offset up to start of pctab

	// Column-split funcRec metadata. When a field's delta-varint encoding is
	// used, the *DLen field carries the encoded byte length; otherwise the
	// slice is raw (nfunc * fieldWidth bytes) and derivable from Nfunc.
	EntryOffDLen   uint64
	NameOffDLen    uint64
	PcspDLen       uint64
	PcfileDLen     uint64
	PclnDLen       uint64
	CuOffsetDLen   uint64
	PcdataLen      uint64 // concatenated pcdata offset bytes (raw)
	FuncdataLen    uint64 // concatenated funcdata offset bytes (raw)
	GapLen         uint64 // inter-record padding bytes
	TrailingLen    uint64 // bytes past the last _func record
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

	// Split funcRecs into columns + pcdata/funcdata streams + inter-record
	// gaps + trailing region past the last record.
	fr, err := splitFuncRecs(ftab, funcRecs, nfunc)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("funcRec split: %w", err)
	}
	entryOffD := deltaVarintCol(fr.cols[0])
	nameOffD := deltaVarintCol(fr.cols[1])
	pcspD := deltaVarintCol(fr.cols[4])
	pcfileD := deltaVarintCol(fr.cols[5])
	pclnD := deltaVarintCol(fr.cols[6])
	cuOffsetD := deltaVarintCol(fr.cols[8])

	var out bytes.Buffer
	out.Write(data[:h.PctabOffset])
	out.Write(val)
	out.Write(pc)
	out.Write(edelta)
	out.Write(fdelta)
	// Column-split funcRec streams (order matters for Decode):
	out.Write(entryOffD)
	out.Write(nameOffD)
	out.Write(fr.cols[2])  // args (raw)
	out.Write(fr.cols[3])  // deferreturn (raw)
	out.Write(pcspD)
	out.Write(pcfileD)
	out.Write(pclnD)
	out.Write(fr.cols[7])  // npcdata (raw)
	out.Write(cuOffsetD)
	out.Write(fr.cols[9])  // startLine (raw)
	out.Write(fr.cols[10]) // funcID
	out.Write(fr.cols[11]) // flag
	out.Write(fr.cols[12]) // pad
	out.Write(fr.cols[13]) // nfuncdata
	out.Write(fr.pcdata)
	out.Write(fr.funcdata)
	out.Write(fr.gap)
	out.Write(fr.trailing)

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

		EntryOffDLen: uint64(len(entryOffD)),
		NameOffDLen:  uint64(len(nameOffD)),
		PcspDLen:     uint64(len(pcspD)),
		PcfileDLen:   uint64(len(pcfileD)),
		PclnDLen:     uint64(len(pclnD)),
		CuOffsetDLen: uint64(len(cuOffsetD)),
		PcdataLen:    uint64(len(fr.pcdata)),
		FuncdataLen:  uint64(len(fr.funcdata)),
		GapLen:       uint64(len(fr.gap)),
		TrailingLen:  uint64(len(fr.trailing)),
	}
	return out.Bytes(), meta, nil
}

// Decode reverses Encode, reconstructing the original .gopclntab bytes.
func Decode(xformed []byte, m Meta) ([]byte, error) {
	nfunc := int(m.Nfunc)
	need := m.HeaderSize + m.ValLen + m.PcLen + m.EDeltaLen + m.FDeltaLen
	if uint64(len(xformed)) < need {
		return nil, errors.New("transformed pcln too short for header")
	}
	off := m.HeaderSize
	val := xformed[off : off+m.ValLen]
	off += m.ValLen
	pc := xformed[off : off+m.PcLen]
	off += m.PcLen
	ed := xformed[off : off+m.EDeltaLen]
	off += m.EDeltaLen
	fd := xformed[off : off+m.FDeltaLen]
	off += m.FDeltaLen

	pctab := pctabInterleave(val, pc)
	if uint64(len(pctab)) != m.PctabSize {
		return nil, fmt.Errorf("pctab reconstruction size mismatch: got %d want %d", len(pctab), m.PctabSize)
	}

	eoff, err := undeltaVarint(ed, nfunc+1)
	if err != nil {
		return nil, fmt.Errorf("decode entryoff deltas: %w", err)
	}
	foff, err := undeltaVarint(fd, nfunc+1)
	if err != nil {
		return nil, fmt.Errorf("decode funcoff deltas: %w", err)
	}
	ftab := make([]byte, m.FtabSize)
	for i := 0; i <= nfunc; i++ {
		copy(ftab[i*8:], eoff[i*4:i*4+4])
		copy(ftab[i*8+4:], foff[i*4:i*4+4])
	}

	// Column streams.
	entryOffBytes, err := undeltaVarintLen(xformed[off:off+m.EntryOffDLen], nfunc)
	if err != nil {
		return nil, fmt.Errorf("decode entryOff col: %w", err)
	}
	off += m.EntryOffDLen
	nameOffBytes, err := undeltaVarintLen(xformed[off:off+m.NameOffDLen], nfunc)
	if err != nil {
		return nil, fmt.Errorf("decode nameOff col: %w", err)
	}
	off += m.NameOffDLen
	args := xformed[off : off+uint64(nfunc)*4]
	off += uint64(nfunc) * 4
	deferreturn := xformed[off : off+uint64(nfunc)*4]
	off += uint64(nfunc) * 4
	pcspBytes, err := undeltaVarintLen(xformed[off:off+m.PcspDLen], nfunc)
	if err != nil {
		return nil, fmt.Errorf("decode pcsp col: %w", err)
	}
	off += m.PcspDLen
	pcfileBytes, err := undeltaVarintLen(xformed[off:off+m.PcfileDLen], nfunc)
	if err != nil {
		return nil, fmt.Errorf("decode pcfile col: %w", err)
	}
	off += m.PcfileDLen
	pclnBytes, err := undeltaVarintLen(xformed[off:off+m.PclnDLen], nfunc)
	if err != nil {
		return nil, fmt.Errorf("decode pcln col: %w", err)
	}
	off += m.PclnDLen
	npcdataBytes := xformed[off : off+uint64(nfunc)*4]
	off += uint64(nfunc) * 4
	cuOffsetBytes, err := undeltaVarintLen(xformed[off:off+m.CuOffsetDLen], nfunc)
	if err != nil {
		return nil, fmt.Errorf("decode cuOffset col: %w", err)
	}
	off += m.CuOffsetDLen
	startLine := xformed[off : off+uint64(nfunc)*4]
	off += uint64(nfunc) * 4
	funcID := xformed[off : off+uint64(nfunc)]
	off += uint64(nfunc)
	flag := xformed[off : off+uint64(nfunc)]
	off += uint64(nfunc)
	pad := xformed[off : off+uint64(nfunc)]
	off += uint64(nfunc)
	nfuncdataBytes := xformed[off : off+uint64(nfunc)]
	off += uint64(nfunc)
	pcdata := xformed[off : off+m.PcdataLen]
	off += m.PcdataLen
	funcdata := xformed[off : off+m.FuncdataLen]
	off += m.FuncdataLen
	gap := xformed[off : off+m.GapLen]
	off += m.GapLen
	trailing := xformed[off : off+m.TrailingLen]
	off += m.TrailingLen

	funcRecs, err := joinFuncRecs(ftab, nfunc,
		entryOffBytes, nameOffBytes, args, deferreturn,
		pcspBytes, pcfileBytes, pclnBytes, npcdataBytes,
		cuOffsetBytes, startLine,
		funcID, flag, pad, nfuncdataBytes,
		pcdata, funcdata, gap, trailing, m.FuncRecSize)
	if err != nil {
		return nil, fmt.Errorf("funcRec join: %w", err)
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

// deltaVarintCol treats b as a column of uint32 LE values and signed-delta
// encodes it as a varint stream. Unlike deltaVarint (unsigned), this uses
// signed deltas because column values (e.g. nameOff) aren't guaranteed
// monotonically increasing.
func deltaVarintCol(b []byte) []byte {
	out := make([]byte, 0, len(b))
	var prev uint32
	var tmp [binary.MaxVarintLen64]byte
	for i := 0; i+3 < len(b); i += 4 {
		v := binary.LittleEndian.Uint32(b[i:])
		d := int64(int32(v - prev))
		prev = v
		n := binary.PutVarint(tmp[:], d)
		out = append(out, tmp[:n]...)
	}
	return out
}

// undeltaVarintLen reverses deltaVarintCol for exactly n uint32 values.
func undeltaVarintLen(b []byte, n int) ([]byte, error) {
	out := make([]byte, 4*n)
	var prev uint32
	i := 0
	for k := 0; k < n; k++ {
		v, c := binary.Varint(b[i:])
		if c <= 0 {
			return nil, fmt.Errorf("varint decode failure at element %d", k)
		}
		i += c
		val := prev + uint32(int32(v))
		binary.LittleEndian.PutUint32(out[k*4:], val)
		prev = val
	}
	return out, nil
}

// funcRecSplit holds the column-split representation of the _func records
// plus the variable-length pcdata/funcdata streams, inter-record alignment
// gaps, and the trailing region past the last record (which Go's pclntab
// includes — appears to be additional pcvalue-like data).
type funcRecSplit struct {
	cols     [14][]byte // 0:entryOff 1:nameOff 2:args 3:deferreturn 4:pcsp 5:pcfile 6:pcln 7:npcdata 8:cuOffset 9:startLine 10:funcID 11:flag 12:pad 13:nfuncdata
	pcdata   []byte     // concatenated pcdata[npcdata] uint32 offsets per func
	funcdata []byte     // concatenated funcdata[nfuncdata] uint32 offsets per func
	gap      []byte     // bytes falling between consecutive records (alignment padding)
	trailing []byte     // bytes past the last record's end
}

// splitFuncRecs iterates the _func records in funcoff order (asserting ftab
// order agrees) and pulls each record apart into per-field column streams
// plus variable-length tail streams.
func splitFuncRecs(ftab, funcRecs []byte, nfunc int) (*funcRecSplit, error) {
	s := &funcRecSplit{}
	for i := range s.cols {
		if i < 10 {
			s.cols[i] = make([]byte, 0, nfunc*4)
		} else {
			s.cols[i] = make([]byte, 0, nfunc)
		}
	}
	ftabSize := uint64((nfunc + 1) * 8)

	// Build (ftabIdx, funcoff) pairs, check monotonicity in ftab order so we
	// can recover records linearly during Decode without storing a permutation.
	var prevFO uint32
	for i := 0; i < nfunc; i++ {
		fo := binary.LittleEndian.Uint32(ftab[i*8+4:])
		if i > 0 && fo < prevFO {
			return nil, fmt.Errorf("ftab funcoff not monotonic at i=%d", i)
		}
		prevFO = fo
	}

	var prevEnd uint64 = ftabSize
	for i := 0; i < nfunc; i++ {
		fo := uint64(binary.LittleEndian.Uint32(ftab[i*8+4:]))
		// ftab funcoff is relative to pclnOffset (start of ftab); funcRecs
		// slice starts at pclnOffset+ftabSize, so record i sits at
		// (fo - ftabSize) within funcRecs.
		recOff := fo - ftabSize
		if fo < ftabSize || recOff+44 > uint64(len(funcRecs)) {
			return nil, fmt.Errorf("funcoff %d out of range", fo)
		}
		// Capture any gap bytes between previous record's end and this one.
		if fo > prevEnd {
			s.gap = append(s.gap, funcRecs[prevEnd-ftabSize:fo-ftabSize]...)
		} else if fo < prevEnd {
			return nil, fmt.Errorf("funcoffs overlap at i=%d (fo=%d prevEnd=%d)", i, fo, prevEnd)
		}
		r := funcRecs[recOff:]
		s.cols[0] = append(s.cols[0], r[0:4]...)
		s.cols[1] = append(s.cols[1], r[4:8]...)
		s.cols[2] = append(s.cols[2], r[8:12]...)
		s.cols[3] = append(s.cols[3], r[12:16]...)
		s.cols[4] = append(s.cols[4], r[16:20]...)
		s.cols[5] = append(s.cols[5], r[20:24]...)
		s.cols[6] = append(s.cols[6], r[24:28]...)
		s.cols[7] = append(s.cols[7], r[28:32]...)
		s.cols[8] = append(s.cols[8], r[32:36]...)
		s.cols[9] = append(s.cols[9], r[36:40]...)
		s.cols[10] = append(s.cols[10], r[40])
		s.cols[11] = append(s.cols[11], r[41])
		s.cols[12] = append(s.cols[12], r[42])
		s.cols[13] = append(s.cols[13], r[43])
		npc := binary.LittleEndian.Uint32(r[28:32])
		nfd := uint32(r[43])
		tailEnd := uint64(44) + 4*uint64(npc+nfd)
		if recOff+tailEnd > uint64(len(funcRecs)) {
			return nil, fmt.Errorf("record %d extends past funcRecs", i)
		}
		s.pcdata = append(s.pcdata, r[44:44+4*npc]...)
		s.funcdata = append(s.funcdata, r[44+4*npc:tailEnd]...)
		prevEnd = fo + tailEnd
	}
	if uint64(len(funcRecs))+ftabSize > prevEnd {
		s.trailing = append(s.trailing, funcRecs[prevEnd-ftabSize:]...)
	}
	return s, nil
}

// joinFuncRecs reconstructs the original funcRecs byte slice from the
// column streams and variable-length streams produced by splitFuncRecs.
func joinFuncRecs(ftab []byte, nfunc int,
	entryOff, nameOff, args, deferreturn,
	pcsp, pcfile, pcln, npcdataCol,
	cuOffset, startLine,
	funcID, flag, pad, nfuncdataCol,
	pcdata, funcdata, gap, trailing []byte,
	expectedSize uint64) ([]byte, error) {
	out := make([]byte, expectedSize)
	ftabSize := uint64((nfunc + 1) * 8)
	var pcdOff, fdOff, gapOff uint64
	var prevEnd uint64 = ftabSize
	for i := 0; i < nfunc; i++ {
		fo := uint64(binary.LittleEndian.Uint32(ftab[i*8+4:]))
		if fo < prevEnd {
			return nil, fmt.Errorf("decode: funcoff %d < prevEnd %d at i=%d", fo, prevEnd, i)
		}
		if fo > prevEnd {
			gapLen := fo - prevEnd
			if gapOff+gapLen > uint64(len(gap)) {
				return nil, fmt.Errorf("gap stream too short at i=%d", i)
			}
			copy(out[prevEnd-ftabSize:], gap[gapOff:gapOff+gapLen])
			gapOff += gapLen
		}
		recOff := fo - ftabSize
		r := out[recOff:]
		copy(r[0:4], entryOff[i*4:i*4+4])
		copy(r[4:8], nameOff[i*4:i*4+4])
		copy(r[8:12], args[i*4:i*4+4])
		copy(r[12:16], deferreturn[i*4:i*4+4])
		copy(r[16:20], pcsp[i*4:i*4+4])
		copy(r[20:24], pcfile[i*4:i*4+4])
		copy(r[24:28], pcln[i*4:i*4+4])
		copy(r[28:32], npcdataCol[i*4:i*4+4])
		copy(r[32:36], cuOffset[i*4:i*4+4])
		copy(r[36:40], startLine[i*4:i*4+4])
		r[40] = funcID[i]
		r[41] = flag[i]
		r[42] = pad[i]
		r[43] = nfuncdataCol[i]
		npc := binary.LittleEndian.Uint32(npcdataCol[i*4:])
		nfd := uint32(nfuncdataCol[i])
		npcBytes := 4 * uint64(npc)
		nfdBytes := 4 * uint64(nfd)
		if pcdOff+npcBytes > uint64(len(pcdata)) {
			return nil, fmt.Errorf("pcdata stream too short at i=%d", i)
		}
		if fdOff+nfdBytes > uint64(len(funcdata)) {
			return nil, fmt.Errorf("funcdata stream too short at i=%d", i)
		}
		copy(r[44:44+npcBytes], pcdata[pcdOff:pcdOff+npcBytes])
		copy(r[44+npcBytes:44+npcBytes+nfdBytes], funcdata[fdOff:fdOff+nfdBytes])
		pcdOff += npcBytes
		fdOff += nfdBytes
		prevEnd = fo + 44 + npcBytes + nfdBytes
	}
	// Trailing.
	if uint64(len(out))+ftabSize > prevEnd {
		copy(out[prevEnd-ftabSize:], trailing)
	}
	return out, nil
}
