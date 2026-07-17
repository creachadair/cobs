// Package cobs implements [COBS], or Consistent Overload Byte Stuffing, an
// encoding method for byte-oriented data such as packets.
//
// # Overview
//
// COBS is an encoding algorithm that transforms a record (or packet) of
// arbitrary binary data into a representation that excludes NUL (0) bytes. The
// encoded representation will be somewhat longer than the original (the
// "consistent overhead").
//
// Informally, the algorithm works by splitting the input record into "blocks"
// of up to 254 bytes, delimited by size or a zero (NUL) byte. Each block is
// then encoded by writing a single-byte length prefix followed by the non-zero
// bytes of the block, and the encodings are concatenated. The resulting
// encoded output does not contain the NUL delimiter.
//
// # Encoding
//
// A [Writer] writes arbitrary binary records into the COBS encoding. Only the
// baseline encoding is supported, extensions like paired-zero encoding are not
// currently implemented. To write a slice of bytes as a single record, use
// [Writer.WriteData]:
//
//	w := cobs.NewWriter(f)
//	err := w.WriteData(input)
//
// Alternatively, use [Writer.WriteRecord], which accepts a callback:
//
//	err := w.WriteRecord(func(rw io.Writer) error {
//	   _, err := io.Copy(rw, src)
//	   return err
//	})
//
// To append the encoding of a single record to a slice, use [Encode]:
//
//	enc := cobs.Encode(nil, input)
//
// # Decoding
//
// A [Reader] decodes the encoded format, allowing the caller to read back the
// decoded data transparently (provided the input is valid). It implements the
// [io.Reader] interface:
//
//	r := cobs.NewReader(f)
//	record, err := io.ReadAll(r)
//
// See the [Reader.Read] documentation for specific details on how [Reader]
// handles delimiters in its input.
//
// To append the decoding of a single record to a slice, use [Decode]:
//
//	dec, err := cobs.Decode(nil, input)
//
// In case of error, [Decode] returns as much of the input as it was able to
// successfully decode, along with the error.
//
// # Implementation Notes
//
// The [Reader] and [Decode] understand the permitted optimization of not
// encoding the logical trailing zero at the end of input, when the previous
// block was full.  It will accept input in either representation. The [Writer]
// and [Encode] always omit the trailer after a full-size block at EOF.
//
// [COBS]: https://conferences.sigcomm.org/sigcomm/1997/papers/p062.pdf
package cobs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// maxBlockSize is the size in bytes of the largest block that can be written
// to a COBS record.
const maxBlockSize = 254

// ErrUnexpectedNUL is a sentinel error reported by [Reader.Read] or [Decode]
// when it encounters a NUL (0) byte in then encoded input.
var ErrUnexpectedNUL = errors.New("unexpected zero byte")

// ErrEndOfRecord is a sentinel error reported by [Reader.Read] or [Decode]
// when it encounters a NUL (0) byte at the end of a record.
var ErrEndOfRecord = errors.New("end of record")

// A Reader reads and decodes COBS format data from an underlying [io.Reader].
// It implements [io.Reader] over the decoded data.
type Reader struct {
	buf   *bufio.Reader
	csize int  // remaining length of current block
	wantz bool // does the previous block want a trailing zero?
}

// NewReader constructs a new [Reader] that consumes encoded data from r.
// Encoded records are delimited by NUL (0) bytes.
func NewReader(r io.Reader) *Reader { return &Reader{buf: bufio.NewReader(r)} }

// Read implements the [io.Reader] interface to decode the contents of r.
//
// Read reports [ErrUnexpectedNUL] if it observes a NUL (0) byte within the
// encoded input. If it encounters a NUL (0) byte at the end of a complete
// record, it consumes and discards the byte, and reports [ErrEndOfRecord].
// A caller may call Read again to attempt to read a successive record.
//
// Read reports [io.ErrUnexpectedEOF] if it encounters a non-empty incomplete
// record at the end of the input. It only reports [io.EOF] when it discovers
// the end of input at the beginning of a record, in which case it will return
// exactly 0, io.EOF.
func (r *Reader) Read(data []byte) (int, error) {
	var nr int
	for len(data) != 0 {
		// Begin a new block. The loop here is a mild optimization for a sequence
		// of encoded zeroes, where we know we don't need to pull from the buffer.
		for r.csize == 0 {
			bs, err := r.buf.ReadByte() // N.B.: Does not count toward nr
			if err != nil {
				return nr, err
			} else if bs == 0 {
				r.wantz = false
				return nr, ErrEndOfRecord
			}

			// If we had a previous block that wanted a trailing zero, deliver it now.
			// Do this after verifying we're not at EOF, so we omit the trailing zero.
			if r.wantz {
				data[0], data = 0, data[1:]
				nr++
				r.wantz = false
			}

			// If the block size is not completely full, we will need to inject a
			// trailing zero at the end of it (after consuming its contents).
			r.wantz = bs != 0xff
			r.csize = int(bs) - 1
		}

		// Reaching here, r.csize > 0.

		end := min(len(data), r.csize)
		cr, err := r.buf.Read(data[:end])
		nr += cr
		r.csize -= cr

		if errors.Is(err, io.EOF) {
			if r.csize != 0 {
				// Validity check: the last block should be complete.
				return nr, fmt.Errorf("missing %d bytes at EOF: %w", r.csize, io.ErrUnexpectedEOF)
			}
		} else if err != nil {
			return nr, err
		}
		if i := bytes.IndexByte(data[:cr], 0); i >= 0 {
			// Validity check: none of the bytes we consumed should be zero.
			return nr, ErrUnexpectedNUL
		}
		data = data[cr:]
	}
	return nr, nil
}

// DiscardUntilNUL consumes and discards input from the underlying reader until
// a NUL (0) byte is observed or the input ends.  It reports the number of
// bytes discarded, including the NUL. If it reaches the end of the input without
// finding a NUL, it reports [io.EOF].
//
// The purpose of this method is to allow a reader to skip past invalid records
// In Nul-delimited input stream. If it is called while in the middle of
// reading a valid record, any remaining unread sections of that record are
// also discarded.
func (r *Reader) DiscardUntilNUL() (int, error) {
	r.csize, r.wantz = 0, false
	var nr int
	for {
		next, err := r.buf.ReadSlice(0)
		nr += len(next)
		if err == nil { // found a NUL
			return nr, nil
		} else if !errors.Is(err, bufio.ErrBufferFull) {
			return nr, err
		}
		// no NUL yet, keep going
	}
}

// firstZero reports the offset after the first zero byte in data, or len(data)
// if there are no zero bytes in data.
func firstZero(data []byte) int {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1
	}
	return len(data)
}

// A Writer wraps an [io.Writer] to allow encoding and writing COBS format records.
//
// Use [Writer.WriteData] to write a slice of bytes as a single record.
// Use [Writer.WriteRecord] to generate a record from a callback.
type Writer struct{ bw *bufio.Writer }

// NewWriter constructs a new [Writer] that encodes data to w.
func NewWriter(w io.Writer) Writer { return Writer{bw: bufio.NewWriter(w)} }

// WriteData encodes data as a single COBS record. If WriteData succeeds, the
// complete record has been written to the underlying writer.
func (w Writer) WriteData(data []byte) error {
	return w.WriteRecord(func(rw io.Writer) error {
		_, err := rw.Write(data)
		return err
	})
}

// WriteRecord calls do with an [io.Writer]. All data written to that writer
// are encoded into a single COBS record in w.
//
// If do reports an error, WriteRecord reports that error. Any data it wrote
// prior to returning will still be encoded. Otherwise it reports the result of
// encoding the record.  If WriteRecord succeeds, the complete record has been
// written to the underlying writer.
func (w Writer) WriteRecord(do func(io.Writer) error) error {
	rw := newRecordWriter(w.bw)
	werr := do(rw)
	cerr := rw.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// ReadFrom writes the complete contents of r as a single record. It implements
// the [io.ReaderFrom] interface.
func (w Writer) ReadFrom(r io.Reader) (int64, error) {
	var nw int64
	err := w.WriteRecord(func(w io.Writer) error {
		var err error
		nw, err = io.Copy(w, r)
		return err
	})
	return nw, err
}

// WriteNUL writes a single NUL (0) byte to the underlying writer, without encoding.
func (w Writer) WriteNUL() error { return errors.Join(w.bw.WriteByte(0), w.bw.Flush()) }

// A recordWriter encodes and writes COBS format data to an underlying
// [io.Writer].  It implements [io.Writer] to accept unencoded data.
type recordWriter struct {
	buf []byte
	bw  *bufio.Writer
}

var _ io.WriteCloser = (*recordWriter)(nil)

// newRecordWriter constructs a new [Writer] that writes encoded data to w.
// Encoded records are delimited by NUL (0) bytes.
func newRecordWriter(bw *bufio.Writer) *recordWriter {
	return &recordWriter{bw: bw, buf: make([]byte, 0, maxBlockSize)}
}

// Write implements the [io.Writer] interface to encode data to w.
func (w *recordWriter) Write(data []byte) (int, error) {
	var nw int
	for len(data) != 0 {
		// If the buffer is full, or if the data we have so far end in a
		// delimiter, we have a block to write out.
		if len(w.buf) == cap(w.buf) || (len(w.buf) != 0 && w.buf[len(w.buf)-1] == 0) {
			if err := w.flushBlock(); err != nil {
				return nw, err
			}
		}

		// A this point the buffer has space, and we have data to add.
		// Calculate how much we can add to the buffer before it is full or
		// ends in a delimiter.
		fz := firstZero(data)
		nc := min(cap(w.buf)-len(w.buf), fz)
		w.buf = append(w.buf, data[:nc]...)
		data = data[nc:]
		nw += nc
	}
	return nw, nil
}

// flushBlock writes the current contents of w.buf to w.bw.
func (w *recordWriter) flushBlock() error {
	nb := len(w.buf) // number of bytes to write
	if len(w.buf) != 0 && w.buf[len(w.buf)-1] == 0 {
		nb-- // don't emit the zero itself
	}
	if err := w.bw.WriteByte(byte(nb + 1)); err != nil {
		return err
	}

	// An io.Writer is required to report an error if it was unable to write the
	// full amount. If that happens, we're in a broken state, since we've
	// written a partial block and cannot resume.
	_, err := w.bw.Write(w.buf[:nb])
	w.buf = w.buf[:0]
	return err
}

// Close writes any buffered data in w to its underlying writer.
// It reports an error if encoding or flushing fails.
func (w *recordWriter) Close() error {
	if w.bw == nil {
		return errors.New("writer is closed")
	}

	// If the remaining buffered data end in a zero, we must behave as if the
	// input had an additional zero appended, because the decoder will assume
	// the final zero of the encoding is a placeholder.
	trailing := len(w.buf) != 0 && w.buf[len(w.buf)-1] == 0
	berr := w.flushBlock()
	if berr == nil && trailing {
		berr = w.bw.WriteByte(1)
	}
	ferr := w.bw.Flush()
	w.bw = nil
	return errors.Join(berr, ferr)
}

// Encode appends the COBS encoding of src to dst, and returns the resulting
// slice.  If dst has sufficient capacity for the encoding, Encode does not
// allocate memory.  See [EncodingLen] and [MaxEncodingLen]. The dst may be
// nil, but src and dst must not overlap.
func Encode(dst, src []byte) []byte {
	if len(src) == 0 {
		return append(dst, 1)
	}
	for len(src) != 0 {
		first, rest, hasZero := firstBlock(src)
		dst = append(dst, byte(len(first)+1))
		dst = append(dst, first...)
		if len(rest) == 0 && hasZero {
			dst = append(dst, 1)
		}
		src = rest
	}
	return dst
}

// EncodingLen reports the (exact) length of the COBS encoding of data without
// allocating memory or constructing the encoding. It reads all of data, but
// does not modify it.
func EncodingLen(data []byte) int {
	if len(data) == 0 {
		return 1
	}
	var size int
	for len(data) != 0 {
		first, rest, hasZero := firstBlock(data)
		size += 1 + len(first) // length + content
		if len(rest) == 0 && hasZero {
			size++ // implicit trailer
		}
		data = rest
	}
	return size
}

// Decode decodes src as a COBS record and appends the result to dst, returning
// the resulting slice.
//
// Decode reports [ErrUnexpectedNUL] if it observes a NUL (0) byte within the
// encoded input. If it encounters a NUL (0) byte at the end of a complete (or
// empty) record, it reports [ErrEndOfRecord]. If a record is truncated at the
// end of src, it reports [io.ErrUnexpectedEOF]. In case of error, Decode
// reports any data successfully decoded along with the error.
//
// If dst has sufficient capacity for the decoded result, Decode does not
// allocate memory.  A destination buffer as big as the input is always
// sufficient.  The src and dst must not overlap.
func Decode(dst, src []byte) ([]byte, error) {
	for len(src) != 0 {
		bs := int(src[0])
		if bs == 0 {
			return dst, ErrEndOfRecord
		} else if bs > len(src) {
			dst = append(dst, src[1:]...) // whatever is left
			return dst, fmt.Errorf("missing %d bytes at EOF: %w", bs-len(src), io.ErrUnexpectedEOF)
		}
		first, rest := src[1:bs], src[bs:]
		dst = append(dst, first...)
		if i := bytes.IndexByte(first, 0); i >= 0 {
			return dst, ErrUnexpectedNUL
		}
		if len(rest) != 0 && rest[0] != 0 && len(first) != maxBlockSize {
			dst = append(dst, 0)
		}
		src = rest
	}
	return dst, nil
}

// MaxEncodingLen reports the maximum possible length of a COBS encoding of an
// n-byte input.  The actual encoded length depends on the content, so this may
// over-estimate by an amount.
func MaxEncodingLen(n int) int {
	// The worst-case for the encoding is if the entire input is full-length
	// non-zero blocks (except maybe the last), each immediately followed by a
	// zero.  Each expands by +1 length byte and an explicitly encoded 0, and we
	// have to encode a trailing zero.
	nb, rem := n/maxBlockSize, n%maxBlockSize
	return nb*(maxBlockSize+2) + rem + 2
}

// firstBlock returns two slices into data, comprising the first encodable
// block of the input (not including its trailing zero, if it had one) and the
// remainder of the data. The hasZero flag is whether the first block had a
// trailing zero.
func firstBlock(data []byte) (block, rest []byte, hasZero bool) {
	block = data[:min(maxBlockSize, len(data))]
	i := bytes.IndexByte(block, 0)
	if i < 0 {
		return block, data[len(block):], false
	}
	return block[:i], data[i+1:], true // +1 to skip the zero
}
