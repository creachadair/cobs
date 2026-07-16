package cobs_test

import (
	"bytes"
	"errors"
	"io"
	mrand "math/rand/v2"
	"strings"
	"testing"

	"github.com/creachadair/cobs"
	"github.com/creachadair/mds/mtest"
	"github.com/google/go-cmp/cmp"
)

var (
	_ io.ReaderFrom = cobs.Writer{}
	_ io.Reader     = (*cobs.Reader)(nil)
)

var full = strings.Repeat("X", 254)

func mustRead(t *testing.T, input string) string {
	t.Helper()
	r := cobs.NewReader(strings.NewReader(input))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("Read %q failed: %v", input, err)
	}
	return string(got)
}

func mustWrite(t *testing.T, input string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := cobs.NewWriter(&buf).WriteData([]byte(input)); err != nil {
		t.Fatalf("Write %q failed: %v", input, err)
	}
	return buf.String()
}

func TestDecoding(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		// Encodings for empty.
		{"", ""},
		{"\x01", ""}, // encoded logical trailing zero

		// Isolated zeroes.
		{"\x01\x01", "\x00"},
		{"\x01\x01\x01", "\x00\x00"},
		{"\x01\x01\x01\x01", "\x00\x00\x00"},

		// Zeroes (or not) at the end of input.
		{"\x06apple", "apple"},
		{"\x06apple\x01", "apple\x00"},

		// Zeroes at the start of input.
		{"\x01\x05pear", "\x00pear"},

		// Zeroes on either side of the input.
		{"\x01\x05plum\x01", "\x00plum\x00"},

		// Mixed stuff.
		{"\x06apple\x05pear\x05plum\x07cherry", "apple\x00pear\x00plum\x00cherry"},
		{"\x0bapple\npear", "apple\npear"},
		{"\x05pear\x01\x05plum", "pear\x00\x00plum"},

		// Full-length blocks (254 bytes).
		{"\xff" + full, full},          // omitted trailer (permitted)
		{"\xff" + full + "\x01", full}, // explicit trailer

		// Handle explicitly-encoded zero after EOF.
		{"\xff" + full + "\x01\x01", full + "\x00"},
		{"\x05kiwi\xff" + full, "kiwi\x00" + full},
		{"\xff" + full + "\x05kiwi\x01", full + "kiwi\x00"},
		{"\xff" + full + "\x01\x05kiwi\x01", full + "\x00kiwi\x00"},
		{"\xff" + full + "\xff" + full + "\x01\xff" + full, full + full + "\x00" + full},
		{"\xff" + full + "\x01\xff" + full, full + "\x00" + full},
	}
	t.Run("Reader", func(t *testing.T) {
		for _, tc := range tests {
			got := mustRead(t, tc.input)
			if got != tc.want {
				t.Errorf("Read %q:\n got %q,\nwant %q", tc.input, got, tc.want)
			}
		}
	})
	t.Run("Decode", func(t *testing.T) {
		for _, tc := range tests {
			got, err := cobs.Decode(nil, []byte(tc.input))
			if err != nil {
				t.Fatalf("Decode %q: unexpected error: %v", tc.input, err)
			}
			if string(got) != tc.want {
				t.Errorf("Decode %q:\n got %q,\nwant %q", tc.input, got, tc.want)
			}
		}
	})
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer // target for Writer
	var dst []byte       // target for Encode
	var inSize int       // total input bytes, excluding delimiters
	var want []string    // record values generated

	w := cobs.NewWriter(&buf)
	for _, n := range []int{7, 1, 0, 2, 3, 5, 10, 15, 21, 37, 50, 254, 300, 400, 2000} {
		msg := make([]byte, n)
		mtest.Random(msg)
		want = append(want, string(msg))
		inSize += len(msg)

		if err := w.WriteData(msg); err != nil {
			t.Fatalf("Write record %d: %v", n, err)
		}
		if err := w.WriteNUL(); err != nil {
			t.Fatalf("WriteNUL %d failed: %v", n, err)
		}

		dst = cobs.Encode(dst, msg)
		dst = append(dst, 0)
	}
	w.WriteNUL()
	dst = append(dst, 0)
	want = append(want, "")

	t.Logf("Input is %d records totaling %d bytes", len(want), inSize)
	t.Logf("Writer output is %d bytes", buf.Len())
	t.Logf("Encode output is %d bytes", len(dst))
	if buf.Len() != len(dst) {
		t.Errorf("Encoding lengths differ: %d from writer, %d from encode", buf.Len(), len(dst))
	}

	var got []string
	r := cobs.NewReader(&buf)
	for {
		next, err := io.ReadAll(r)
		if err == nil {
			break // end of input, via io.EOF consumed in io.ReadAll
		} else if !errors.Is(err, cobs.ErrEndOfRecord) {
			t.Fatalf("Read failed: %v", err)
		}
		got = append(got, string(next))

		first, rest, _ := bytes.Cut(dst, []byte{0})
		msg, err := cobs.Decode(nil, first)
		if err != nil {
			t.Errorf("Decode %q: unexpected error: %v", first, err)
		} else if string(msg) != string(next) {
			t.Errorf("Decode %q: got %q, want %q", first, msg, next)
		}
		dst = rest
	}

	// Verify that we fully consumed all the input.
	if buf.Len() != 0 {
		t.Errorf("Found %d unconsumed bytes after reading: %q", buf.Len(), buf.Bytes())
	}
	if len(dst) != 0 {
		t.Errorf("Found %d unconsumed bytes after decoding: %q", len(dst), dst)
	}

	// Verify that we got the same stuff we wrote in.
	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("Round trip result (-got, +want):\n%s", diff)
	}
}

func TestReaderFrom(t *testing.T) {
	var buf bytes.Buffer
	w := cobs.NewWriter(&buf)

	const input = "full plate\x00and packing steel\x00\x00"
	n, err := w.ReadFrom(strings.NewReader(input))
	if err != nil {
		t.Errorf("ReadFrom: unexpected error: %v", err)
	}
	if n != int64(len(input)) {
		t.Errorf("ReadFrom: got %d bytes, want %d", n, len(input))
	}
	t.Logf("Input:   %q (%d bytes)", input, len(input))
	t.Logf("Encoded: %q (%d bytes)", buf.Bytes(), buf.Len())

	dec, err := cobs.Decode(nil, buf.Bytes())
	if err != nil {
		t.Errorf("Decode: unexpected error: %v", err)
	}
	if string(dec) != input {
		t.Errorf("Decode: got %q, want %q", dec, input)
	}
}

func TestMaxEncodingLen(t *testing.T) {
	input := make([]byte, 9999)
	est := cobs.MaxEncodingLen(len(input))
	buf := make([]byte, 0, est)
	act := len(cobs.Encode(buf[:0], input))
	t.Logf("Input size:        %d bytes", len(input))
	t.Logf("Max encoding size: %d bytes", est)
	t.Logf("Actual encoding:   %d bytes", act)

	t.Run("NoAlloc", func(t *testing.T) {
		const numRuns = 5000
		na := testing.AllocsPerRun(numRuns, func() {
			mtest.Random(input)
			_ = cobs.Encode(buf[:0], input)
		})
		if na != 0 {
			t.Fatalf("Saw %f allocations, want 0", na)
		}
	})
}

func TestEncodingLen(t *testing.T) {
	for range 1000 {
		n := mrand.N(600)
		input := make([]byte, n)
		mtest.Random(input)

		est := cobs.EncodingLen(input)
		act := len(cobs.Encode(nil, input))
		if est != act {
			t.Errorf("EncodingLen(%q): got %d, want %d", input, est, act)
		}
	}
}

func TestEncoding(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		// Empty encodes to a logical trailing zero only.
		{"", "\x01"},

		// Trailing zeroes get properly escaped.
		{"\x00", "\x01\x01"},
		{"\x00\x00", "\x01\x01\x01"},

		// No zeroes.
		{"apple", "\x06apple"},

		// Zeroes on either end.
		{"apple\x00", "\x06apple\x01"},
		{"\x00pear", "\x01\x05pear"},

		// Zeroes in the middle.
		{"apple\x00pear\x00quince", "\x06apple\x05pear\x07quince"},

		// Zeroes all around (including multiples).
		{"a\x00b\x00c\x00d\x00e\x00f", "\x02a\x02b\x02c\x02d\x02e\x02f"},
		{"\x00pear\x00apple\x00", "\x01\x05pear\x06apple\x01"},
		{"\x00plum\x00\x00cherry\x00", "\x01\x05plum\x01\x07cherry\x01"},
		{"\x00\x00quince\x00\x00", "\x01\x01\x07quince\x01\x01"},

		// Full-length blocks (254 bytes).
		{full, "\xff" + full}, // note omitted trailer (permitted)

		// A full-length block does not imply a trailing zero, so if the input
		// has a zero following a full block, it must be explicitly encoded.
		// Note that the first \0x01 is the block encoding the \x00 itself,
		// and the second \x01 encodes the (required) logical trailing zero.
		{full + "\x00", "\xff" + full + "\x01\x01"},

		// A mix of sizes, including full and non-full blocks.
		{full + "\x00" + full[:len(full)-2], "\xff" + full + "\x01\xfd" + full[:len(full)-2]},
		{full + "\x00" + full[:len(full)-2] + "\x00", "\xff" + full + "\x01\xfd" + full[:len(full)-2] + "\x01"},
		{full + "\x00" + full + "\x00" + full + "\x00", "\xff" + full + "\x01\xff" + full + "\x01\xff" + full + "\x01\x01"},
	}
	t.Run("Writer", func(t *testing.T) {
		for _, tc := range tests[len(tests)-1:] {
			got := mustWrite(t, tc.input)
			if got != tc.want {
				t.Errorf("Write %q:\ngot  %q,\nwant %q", tc.input, got, tc.want)
			}
			if cmp := mustRead(t, got); cmp != tc.input {
				t.Errorf("Round trip %q:\ngot  %q,\nwant %q", got, cmp, tc.input)
			}
		}
	})
	t.Run("Encode", func(t *testing.T) {
		for _, tc := range tests {
			mlen := cobs.MaxEncodingLen(len(tc.input))
			elen := cobs.EncodingLen([]byte(tc.input))

			got := cobs.Encode(nil, []byte(tc.input))
			if len(got) != elen {
				t.Errorf("Length %q: got %d, want %d", tc.input, len(got), elen)
			}
			if mlen < elen {
				t.Errorf("Max length %q: got %d, want at least %d", tc.input, mlen, elen)
			}
			if string(got) != tc.want {
				t.Errorf("Encode %q: got %q, want %q", tc.input, got, tc.want)
			}
		}
	})
}

func TestErrors(t *testing.T) {
	full := strings.Repeat("X", 254)
	tests := []struct {
		input string
		want  string
		err   error
	}{
		{"\x00", "", cobs.ErrEndOfRecord},
		{"\x01\x00", "", cobs.ErrEndOfRecord},
		{"\x01\x01\x00", "\x00", cobs.ErrEndOfRecord},
		{"\x04abc\x00", "abc", cobs.ErrEndOfRecord},
		{"\x04pd", "pd", io.ErrUnexpectedEOF},
		{"\x06apple\x05pea", "apple\x00pea", io.ErrUnexpectedEOF},
		{"\x05a\x00bc", "a", cobs.ErrUnexpectedNUL},
		{"\xff" + full[:len(full)-5], full[:len(full)-5], io.ErrUnexpectedEOF},
		{"\xff" + full + "\x03a\x00", full + "a", cobs.ErrUnexpectedNUL},
	}
	t.Run("Decode", func(t *testing.T) {
		for _, tc := range tests {
			dec, err := cobs.Decode(nil, []byte(tc.input))
			if !errors.Is(err, tc.err) {
				t.Errorf("Decode %q: got error %v, want %v", tc.input, err, tc.err)
			}
			if string(dec) != tc.want {
				t.Errorf("Decode %q output: got %q, want %q", tc.input, dec, tc.want)
			}
		}
	})
	t.Run("Reader", func(t *testing.T) {
		for _, tc := range tests {
			got, err := io.ReadAll(cobs.NewReader(strings.NewReader(tc.input)))
			if !errors.Is(err, tc.err) {
				t.Errorf("Read %q: got error %v, want %v", tc.input, err, tc.err)
			}
			if string(got) != tc.want {
				t.Errorf("Read %q output: got %q, want %q", tc.input, got, tc.want)
			}
		}
	})
}
