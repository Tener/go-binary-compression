package pcln

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type decodeStream struct {
	*bufio.Reader
	limit *io.LimitedReader
}

func (s *decodeStream) remaining() int64 {
	return s.limit.N + int64(s.Buffered())
}

type deltaStream struct {
	*decodeStream
	prev   uint32
	signed bool
}

func (s *deltaStream) next() (uint32, error) {
	var delta uint32
	if s.signed {
		v, err := binary.ReadVarint(s)
		if err != nil {
			return 0, err
		}
		if v < math.MinInt32 || v > math.MaxInt32 {
			return 0, fmt.Errorf("signed delta out of range: %d", v)
		}
		delta = uint32(int32(v))
	} else {
		v, err := binary.ReadUvarint(s)
		if err != nil {
			return 0, err
		}
		if v > math.MaxUint32 {
			return 0, fmt.Errorf("unsigned delta out of range: %d", v)
		}
		delta = uint32(v)
	}
	s.prev += delta
	return s.prev, nil
}

// DecodeTo reconstructs .gopclntab into w using fixed-size buffers.
// r must support independent reads of the column streams. size is the
// exact transformed section length. Errors may leave partial output in w.
func DecodeTo(w io.Writer, r io.ReaderAt, size int64, m Meta) error {
	if size < 0 || m.Nfunc > math.MaxInt64/8-1 || m.FtabSize != (m.Nfunc+1)*8 {
		return fmt.Errorf("invalid pcln size or function count")
	}
	total := uint64(0)
	for _, n := range []uint64{m.HeaderSize, m.PctabSize, m.FtabSize, m.FuncRecSize} {
		if n > math.MaxInt64-total {
			return fmt.Errorf("pcln output size overflow")
		}
		total += n
	}
	if m.ValLen > m.PctabSize || m.PcLen != m.PctabSize-m.ValLen {
		return fmt.Errorf("pctab stream sizes do not match")
	}
	recordBytes := m.FuncRecSize
	if m.Nfunc > recordBytes/44 {
		return fmt.Errorf("function records too short")
	}
	recordBytes -= m.Nfunc * 44
	for _, n := range []uint64{m.PcdataLen, m.FuncdataLen, m.GapLen, m.TrailingLen} {
		if n > recordBytes {
			return fmt.Errorf("function record stream sizes do not match")
		}
		recordBytes -= n
	}
	if recordBytes != 0 {
		return fmt.Errorf("function record stream sizes do not match")
	}
	lengths := []uint64{
		m.HeaderSize, m.ValLen, m.PcLen, m.EDeltaLen, m.FDeltaLen,
		m.EntryOffDLen, m.NameOffDLen, m.Nfunc * 4, m.Nfunc * 4,
		m.PcspDLen, m.PcfileDLen, m.PclnDLen, m.Nfunc * 4,
		m.CuOffsetDLen, m.Nfunc * 4, m.Nfunc, m.Nfunc, m.Nfunc, m.Nfunc,
		m.PcdataLen, m.FuncdataLen, m.GapLen, m.TrailingLen,
	}
	left := uint64(size)
	for _, n := range lengths {
		if n > left {
			return fmt.Errorf("transformed pcln too short")
		}
		left -= n
	}
	if left != 0 {
		return fmt.Errorf("unexpected bytes after transformed pcln")
	}

	streams := make([]*decodeStream, len(lengths))
	var off int64
	for i, n := range lengths {
		limit := &io.LimitedReader{R: io.NewSectionReader(r, off, int64(n)), N: int64(n)}
		streams[i] = &decodeStream{bufio.NewReaderSize(limit, 8*1024), limit}
		off += int64(n)
	}
	out := bufio.NewWriterSize(w, 32*1024)
	scratch := make([]byte, 32*1024)
	copyStream := func(s *decodeStream, n uint64) error {
		if n > uint64(s.remaining()) {
			return fmt.Errorf("pcln stream too short")
		}
		for n > 0 {
			take := min(n, uint64(len(scratch)))
			buf := scratch[:int(take)]
			if _, err := io.ReadFull(s, buf); err != nil {
				return err
			}
			if _, err := out.Write(buf); err != nil {
				return err
			}
			n -= take
		}
		return nil
	}
	if err := copyStream(streams[0], m.HeaderSize); err != nil {
		return err
	}

	// Interleave varint bytes without interpreting their values. Encode
	// preserves incomplete terminal varints, so DecodeTo does as well.
	val, pc := streams[1], streams[2]
	var pctabBytes uint64
	copyVarint := func(s *decodeStream) (bool, error) {
		first, err := s.ReadByte()
		if err != nil {
			return false, err
		}
		b := first
		for {
			if err := out.WriteByte(b); err != nil {
				return false, err
			}
			pctabBytes++
			if b&0x80 == 0 || s.remaining() == 0 {
				return first != 0, nil
			}
			b, err = s.ReadByte()
			if err != nil {
				return false, err
			}
		}
	}
	for val.remaining() > 0 {
		nonzero, err := copyVarint(val)
		if err != nil {
			return err
		}
		if !nonzero {
			continue
		}
		if pc.remaining() == 0 {
			break
		}
		if _, err := copyVarint(pc); err != nil {
			return err
		}
	}
	if pctabBytes != m.PctabSize {
		return fmt.Errorf("pctab reconstruction size mismatch")
	}

	entries := deltaStream{decodeStream: streams[3]}
	funcs := deltaStream{decodeStream: streams[4]}
	var pair [8]byte
	for i := uint64(0); i <= m.Nfunc; i++ {
		entry, err := entries.next()
		if err != nil {
			return fmt.Errorf("decode entryoff %d: %w", i, err)
		}
		fo, err := funcs.next()
		if err != nil {
			return fmt.Errorf("decode funcoff %d: %w", i, err)
		}
		binary.LittleEndian.PutUint32(pair[:4], entry)
		binary.LittleEndian.PutUint32(pair[4:], fo)
		if _, err := out.Write(pair[:]); err != nil {
			return err
		}
	}
	if streams[3].remaining() != 0 || streams[4].remaining() != 0 {
		return fmt.Errorf("unused ftab deltas")
	}
	// Re-read the funcoff deltas to place records, avoiding a stored ftab.
	fdelta := io.NewSectionReader(r,
		int64(m.HeaderSize+m.ValLen+m.PcLen+m.EDeltaLen), int64(m.FDeltaLen))
	streams[4].limit.N = int64(m.FDeltaLen)
	streams[4].limit.R = fdelta
	streams[4].Reset(streams[4].limit)
	funcs = deltaStream{decodeStream: streams[4]}
	var cols [14]deltaStream
	for i := range cols {
		cols[i] = deltaStream{decodeStream: streams[5+i], signed: true}
	}
	prevEnd := m.FtabSize
	recordEnd := m.FtabSize + m.FuncRecSize
	var record [44]byte
	for i := uint64(0); i < m.Nfunc; i++ {
		fo32, err := funcs.next()
		if err != nil {
			return fmt.Errorf("decode record offset %d: %w", i, err)
		}
		fo := uint64(fo32)
		if fo < prevEnd || fo > recordEnd || recordEnd-fo < 44 {
			return fmt.Errorf("record %d offset out of range: %d", i, fo)
		}
		if err := copyStream(streams[21], fo-prevEnd); err != nil {
			return fmt.Errorf("record %d gap: %w", i, err)
		}
		for c := range cols {
			if c == 0 || c == 1 || c == 4 || c == 5 || c == 6 || c == 8 {
				v, err := cols[c].next()
				if err != nil {
					return fmt.Errorf("record %d column %d: %w", i, c, err)
				}
				binary.LittleEndian.PutUint32(record[c*4:], v)
			} else {
				start, width := c*4, 4
				if c >= 10 {
					start, width = 40+c-10, 1
				}
				if _, err := io.ReadFull(cols[c].decodeStream, record[start:start+width]); err != nil {
					return fmt.Errorf("record %d column %d: %w", i, c, err)
				}
			}
		}
		pcBytes := uint64(binary.LittleEndian.Uint32(record[28:])) * 4
		funcBytes := uint64(record[43]) * 4
		if pcBytes+funcBytes > recordEnd-fo-44 {
			return fmt.Errorf("record %d tail out of range", i)
		}
		if _, err := out.Write(record[:]); err != nil {
			return err
		}
		if err := copyStream(streams[19], pcBytes); err != nil {
			return fmt.Errorf("record %d pcdata: %w", i, err)
		}
		if err := copyStream(streams[20], funcBytes); err != nil {
			return fmt.Errorf("record %d funcdata: %w", i, err)
		}
		prevEnd = fo + 44 + pcBytes + funcBytes
	}
	// The final ftab entry has no function record.
	if _, err := funcs.next(); err != nil {
		return fmt.Errorf("decode final funcoff: %w", err)
	}
	if m.TrailingLen != recordEnd-prevEnd {
		return fmt.Errorf("trailing size mismatch")
	}
	if err := copyStream(streams[22], m.TrailingLen); err != nil {
		return err
	}
	for i, s := range streams {
		if s.remaining() != 0 {
			return fmt.Errorf("unused bytes in pcln stream %d", i)
		}
	}
	return out.Flush()
}
