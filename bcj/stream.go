package bcj

import "io"

// DecodeTo reverses Encode from r into w using a fixed-size buffer.
// size is the exact length of the encoded text section.
func DecodeTo(w io.Writer, r io.Reader, size int64) error {
	if size < 0 {
		return io.ErrUnexpectedEOF
	}
	buf := make([]byte, 64*1024+4)
	var pos uint32
	carry := 0
	for size > 0 {
		n := min(int64(len(buf)-carry), size)
		if _, err := io.ReadFull(r, buf[carry:carry+int(n)]); err != nil {
			return err
		}
		size -= n
		end := carry + int(n)
		i := apply(buf[:end], false, pos)
		if size == 0 {
			i = end
		}
		written, err := w.Write(buf[:i])
		if err != nil {
			return err
		}
		if written != i {
			return io.ErrShortWrite
		}
		pos += uint32(i)
		carry = copy(buf, buf[i:end])
	}
	return nil
}
