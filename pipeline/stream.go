package pipeline

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/Tener/go-binary-compression/bcj"
	"github.com/Tener/go-binary-compression/pcln"
)

// DecodeTo reverses Encode into w using fixed-size buffers. r must support
// random access, and size must be the exact encoded blob length.
// Errors may leave partial output in w.
func DecodeTo(w io.Writer, r io.ReaderAt, size int64) error {
	var env Envelope
	hdrSize := int64(env.Size())
	if size < hdrSize {
		return fmt.Errorf("blob too short for envelope (need %d, got %d)", hdrSize, size)
	}
	if err := binary.Read(io.NewSectionReader(r, 0, hdrSize), binary.LittleEndian, &env); err != nil {
		return fmt.Errorf("read envelope: %w", err)
	}
	bodySize := uint64(size - hdrSize)
	if env.XformLen > bodySize || env.PclnSize > math.MaxInt64-(bodySize-env.XformLen) {
		return fmt.Errorf("invalid transformed or original pcln size")
	}
	rawSize := bodySize - env.XformLen + env.PclnSize
	if env.TextOff > rawSize || env.TextSize > rawSize-env.TextOff ||
		env.PclnOff > rawSize || env.PclnSize > rawSize-env.PclnOff {
		return fmt.Errorf("section outside original file")
	}
	textEnd := env.TextOff + env.TextSize
	pclnEnd := env.PclnOff + env.PclnSize
	if env.TextOff < pclnEnd && env.PclnOff < textEnd {
		return fmt.Errorf("overlapping text and pcln sections")
	}
	restoredPclnSize := uint64(0)
	for _, n := range []uint64{env.PclnMeta.HeaderSize, env.PclnMeta.PctabSize, env.PclnMeta.FtabSize, env.PclnMeta.FuncRecSize} {
		if n > env.PclnSize-restoredPclnSize {
			return fmt.Errorf("pcln metadata size mismatch")
		}
		restoredPclnSize += n
	}
	if restoredPclnSize != env.PclnSize {
		return fmt.Errorf("pcln metadata size mismatch")
	}

	scratch := make([]byte, 32*1024)
	var inputOff, outputOff uint64
	copyRaw := func(n uint64) error {
		if n > bodySize-inputOff {
			return fmt.Errorf("encoded body too short")
		}
		copied, err := io.CopyBuffer(w, io.NewSectionReader(r, hdrSize+int64(inputOff), int64(n)), scratch)
		if err == nil && copied != int64(n) {
			err = io.ErrUnexpectedEOF
		}
		inputOff += n
		outputOff += n
		return err
	}
	decodeText := func() error {
		if err := copyRaw(env.TextOff - outputOff); err != nil {
			return err
		}
		if env.TextSize > bodySize-inputOff {
			return fmt.Errorf("encoded text too short")
		}
		if err := bcj.DecodeTo(w, io.NewSectionReader(r, hdrSize+int64(inputOff), int64(env.TextSize)), int64(env.TextSize)); err != nil {
			return fmt.Errorf("bcj decode: %w", err)
		}
		inputOff += env.TextSize
		outputOff += env.TextSize
		return nil
	}
	decodePcln := func() error {
		if err := copyRaw(env.PclnOff - outputOff); err != nil {
			return err
		}
		if env.XformLen > bodySize-inputOff {
			return fmt.Errorf("encoded pcln too short")
		}
		section := io.NewSectionReader(r, hdrSize+int64(inputOff), int64(env.XformLen))
		if err := pcln.DecodeTo(w, section, int64(env.XformLen), env.PclnMeta); err != nil {
			return fmt.Errorf("pcln decode: %w", err)
		}
		inputOff += env.XformLen
		outputOff += env.PclnSize
		return nil
	}
	first, second := decodeText, decodePcln
	if env.PclnOff < env.TextOff {
		first, second = decodePcln, decodeText
	}
	if err := first(); err != nil {
		return err
	}
	if err := second(); err != nil {
		return err
	}
	return copyRaw(bodySize - inputOff)
}
