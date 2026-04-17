// Package bcj implements an x86-64 Branch/Call/Jump filter, a format-aware
// preprocessing step that makes downstream LZ-family compressors more
// effective on machine code.
//
// The transform rewrites the 32-bit displacement field of every CALL (E8)
// and JMP (E9) instruction so that identical targets have identical byte
// sequences regardless of call-site address. After a compressor decompresses
// the stream, the inverse transform restores the original PC-relative form.
//
// This is a simpler "always-convert" variant of the LZMA-SDK filter: it does
// not apply the top-byte-of-displacement range check and therefore also
// transforms bytes that happen to look like CALL/JMP in data sections.
// That costs a small amount of compression on non-text bytes but is
// trivially reversible because the transform is a position-deterministic
// bijection: (x + pos + 5) - (pos + 5) = x.
package bcj

import "encoding/binary"

// Encode rewrites every E8/E9 opcode's 4-byte operand from PC-relative disp32
// to an absolute offset within buf.
func Encode(buf []byte) {
	apply(buf, true)
}

// Decode reverses Encode.
func Decode(buf []byte) {
	apply(buf, false)
}

func apply(buf []byte, encode bool) {
	n := len(buf)
	for i := 0; i+4 < n; {
		op := buf[i]
		if op != 0xE8 && op != 0xE9 {
			i++
			continue
		}
		v := binary.LittleEndian.Uint32(buf[i+1:])
		var next uint32
		if encode {
			next = v + uint32(i) + 5
		} else {
			next = v - uint32(i) - 5
		}
		binary.LittleEndian.PutUint32(buf[i+1:], next)
		i += 5
	}
}
