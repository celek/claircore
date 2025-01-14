// Package zreader implements a transparently decompressing [io.Reader].
package zreader

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"io"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"
)

//go:generate go run golang.org/x/tools/cmd/stringer -type Compression

// Compression marks the scheme that the original Reader contains.
type Compression int

// Compression constants.
const (
	KindGzip Compression = iota
	KindZstd
	KindBzip2
	KindZlib
	KindNone
)

// Max number of bytes needed to check compression headers. Populated in this
// package's init func to avoid needing to keep some constants manually updated.
var maxSz int

func init() {
	for _, d := range detectors[:] {
		l := len(d.Mask)
		if l > maxSz {
			maxSz = l
		}
	}
}

// Detector is the hook to determine if a Reader contains a certain compression
// scheme.
type detector struct {
	// Mask is a bytemask for the bytes passed to Check.
	Mask []byte
	// Check reports if the byte slice is the header for a given compression
	// scheme.
	//
	// The passed byte size is sliced to the same size of Mask, and has been
	// ANDed pairwise with Mask.
	Check func([]byte) bool
}

// Detectors is the array of detection hooks.
var detectors = [...]detector{
	staticHeader(gzipHeader),
	staticHeader(zstdHeader),
	// Bzip2 header is technically 2 bytes, but the other valid value for byte 3
	// is bzip1-compat format and the fourth byte is required to in a certain
	// range.
	{
		Mask: bytes.Repeat([]byte{0xFF}, 4),
		Check: func(b []byte) bool {
			l := len(bzipHeader)
			return bytes.Equal(bzipHeader, b[:l]) && (b[l] >= '1' && b[l] <= '9')
		},
	},
	// The zlib header is bit-packed, so we need to do something more complex
	// than bytes.Equal.
	{
		Mask: bytes.Repeat([]byte{0xFF}, 6),
		Check: func(b []byte) bool {
			const (
				magic     = 8
				maxWindow = 7
			)
			x := binary.BigEndian.Uint16(b[:2])
			if (b[0]&0x0f != magic) || (b[0]>>4 > maxWindow) || x%31 != 0 {
				return false
			}
			if b[1]&0x20 != 0 {
				ck := binary.BigEndian.Uint32(b[2:])
				if ck != zlibChecksum {
					return false
				}
			}
			return true
		},
	},
}

// StaticHeader is a helper to create a [detector] for has a constant byte
// string.
func staticHeader(h []byte) detector {
	return detector{
		Mask: bytes.Repeat([]byte{0xFF}, len(h)),
		Check: func(b []byte) bool {
			return bytes.Equal(h, b)
		},
	}
}

// Some static header values.
var (
	gzipHeader = []byte{0x1F, 0x8B, 0x08}
	zstdHeader = []byte{0x28, 0xB5, 0x2F, 0xFD}
	bzipHeader = []byte{'B', 'Z', 'h'}
)

// ZlibChecksum is the checksum for zlib stream that does not have a provided
// dictionary. It's impossible to have a pre-decided dictionary without a
// sideband.
var zlibChecksum = adler32.Checksum(nil)

// DetectCompression reports the compression type indicated based on the header
// contained in the passed byte slice.
//
// "CmpNone" is returned if all detectors report false, but it's possible that
// it's just a scheme unsupported by this package.
func detectCompression(b []byte) Compression {
	t := make([]byte, len(b))
	for c, d := range detectors {
		n, l := copy(t, b), len(d.Mask)
		if n < l {
			continue
		}
		t := t[:l]
		for i := range d.Mask {
			t[i] &= d.Mask[i]
		}
		if d.Check(t) {
			return Compression(c)
		}
	}
	return KindNone
}

// Reader returns an [io.ReadCloser] that transparently reads bytes compressed with
// one of the following schemes:
//
//   - gzip
//   - zstd
//   - bzip2
//   - zlib
//
// If the data does not seem to be one of these schemes, a new [io.ReadCloser]
// equivalent to the provided [io.Reader] is returned.
// The provided [io.Reader] is expected to have any necessary cleanup arranged
// by the caller; that is, it will not arrange for a Close method to be called
// if it also implements [io.Closer].
func Reader(r io.Reader) (rc io.ReadCloser, err error) {
	rc, _, err = detect(r)
	return rc, err
}

// Detect follows the same procedure as [Reader], but also reports the detected
// compression scheme.
func Detect(r io.Reader) (io.ReadCloser, Compression, error) {
	return detect(r)
}

// Detect (unexported) does the actual work for both [Detect] and [Reader].
func detect(r io.Reader) (io.ReadCloser, Compression, error) {
	br := bufio.NewReader(r)
	// Populate a buffer with enough bytes to determine what header is at the
	// start of this Reader.
	b, err := br.Peek(maxSz)
	switch {
	case errors.Is(err, nil):
	case errors.Is(err, io.ErrNoProgress):
		return io.NopCloser(br), KindNone, nil
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		// Not enough bytes, just return a reader containing the bytes.
		return io.NopCloser(bytes.NewReader(b)), KindNone, err
	default:
		return nil, KindNone, err
	}

	// Run the detectors.
	//
	// All the return types are a little different, so they're handled in the
	// switch arms.
	switch c := detectCompression(b); c {
	case KindGzip:
		z, err := gzip.NewReader(br)
		return z, c, err
	case KindZstd:
		z, err := zstd.NewReader(br)
		if err != nil {
			return nil, KindNone, err
		}
		return z.IOReadCloser(), c, nil
	case KindBzip2:
		z := bzip2.NewReader(br)
		return io.NopCloser(z), c, nil
	case KindZlib:
		z, err := zlib.NewReader(br)
		return z, c, err
	case KindNone:
		// Return the reconstructed Reader.
	default:
		panic(fmt.Sprintf("programmer error: unknown compression type %v (bytes read: %#v)", c, b))
	}
	return io.NopCloser(br), KindNone, nil
}
