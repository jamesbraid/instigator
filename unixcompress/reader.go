// Copyright (c) 1985, 1986, 1992, 1993
//	The Regents of the University of California.  All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause
//
// Ported from FreeBSD usr.bin/gzip/zuncompress.c (freebsd-src 42b3884); see
// THIRD_PARTY_NOTICES.

// Package unixcompress reads Unix compress(1) data, the ".Z" format: LZW
// with codes growing from 9 bits to a maximum named in the header, and an
// optional block mode in which a clear code resets the table. Go's
// compress/lzw uses a different bit layout.
//
// It accepts exactly what FreeBSD gzip accepts: the header's maximum code
// width must be 12 to 16, and a cut stream decodes as far as it goes. It
// differs only where Go forces it: errors wrap ErrCorrupt and bytes decoded
// before one are still returned, read errors are returned rather than
// taken for end of data, and the C's masked read one byte past a 16-bit
// group is skipped.
package unixcompress

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// Magic is the two-byte signature that starts every compress(1) file.
const Magic = "\x1f\x9d"

// ErrCorrupt is returned (possibly wrapped) when the data is not valid
// compress(1) output. Test for it with errors.Is.
var ErrCorrupt = errors.New("unixcompress: corrupt data")

const (
	bits      = 16   // BITS: the widest code
	bitMask   = 0x1f // BIT_MASK: header byte 2, maximum code width
	blockMask = 0x80 // BLOCK_MASK: header byte 2, block mode
	initBits  = 9    // INIT_BITS
	first     = 257  // FIRST: first free entry in block mode
	clear     = 256  // CLEAR
)

func maxCode(n int) int { return 1<<n - 1 }

// IsCompressed reports whether b begins with the compress(1) magic bytes.
func IsCompressed(b []byte) bool {
	return len(b) >= len(Magic) && b[0] == Magic[0] && b[1] == Magic[1]
}

// reader holds the read side of the C's struct s_zstate.
type reader struct {
	fp  *bufio.Reader
	err error // sticky

	nBits         int
	maxBits       int
	maxCode       int
	maxMaxCode    int
	freeEnt       int
	blockCompress bool
	clearFlg      bool

	prefix [1 << bits]uint16
	suffix [1 << bits]byte

	// stack is de_stack: the current string, last byte first.
	stack   []byte
	finChar byte
	oldCode int
	incode  int

	// gbuf persists between groups: a short final group reads the bytes
	// it lacks from the previous one, as the C does.
	gbuf    [bits]byte
	rOffset int
	size    int
}

// NewReader returns a reader that decompresses compress(1) data from r. It
// reads the three-byte header immediately and returns an error wrapping
// ErrCorrupt if it is missing or invalid. Closing the result does not close
// r.
func NewReader(r io.Reader) (io.ReadCloser, error) {
	z := &reader{fp: bufio.NewReaderSize(r, 64<<10)}
	var header [3]byte
	if _, err := io.ReadFull(z.fp, header[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("%w: short header", ErrCorrupt)
		}
		return nil, err
	}
	if !IsCompressed(header[:]) {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	z.maxBits = int(header[2])
	z.blockCompress = z.maxBits&blockMask != 0
	z.maxBits &= bitMask
	z.maxMaxCode = 1 << z.maxBits
	if z.maxBits > bits || z.maxBits < 12 {
		return nil, fmt.Errorf("%w: code width %d outside 12-%d", ErrCorrupt, z.maxBits, bits)
	}
	z.nBits = initBits
	z.maxCode = maxCode(initBits)
	for code := 255; code >= 0; code-- {
		z.suffix[code] = byte(code)
	}
	z.freeEnt = 256
	if z.blockCompress {
		z.freeEnt = first
	}
	z.oldCode = -1
	return z, nil
}

func (z *reader) Close() error { return nil }

func (z *reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := 0
	for n < len(p) {
		if len(z.stack) > 0 {
			k := min(len(z.stack), len(p)-n)
			for i := 0; i < k; i++ {
				p[n+i] = z.stack[len(z.stack)-1-i]
			}
			z.stack = z.stack[:len(z.stack)-k]
			n += k
			if len(z.stack) == 0 {
				z.newEntry()
			}
			continue
		}
		if z.err != nil {
			break
		}
		z.err = z.decode()
	}
	if n > 0 {
		return n, nil
	}
	return 0, z.err
}

// decode is the top of zread's loop: it reads codes until one produces a
// string, and pushes that string on the stack. It returns io.EOF at the end
// of the data.
func (z *reader) decode() error {
	for {
		code, err := z.getCode()
		if err != nil {
			return err
		}
		if code == clear && z.blockCompress {
			// The C zeroes the prefixes of the 256 literal codes, which
			// are never read.
			z.clearFlg = true
			z.freeEnt = first
			z.oldCode = -1
			continue
		}
		inCode := code

		// Special case for KwKwK string.
		if code >= z.freeEnt {
			if code > z.freeEnt || z.oldCode == -1 {
				return fmt.Errorf("%w: code %d beyond table (%d entries)", ErrCorrupt, code, z.freeEnt)
			}
			z.stack = append(z.stack, z.finChar)
			code = z.oldCode
		}

		// Generate output characters in reverse order. Each prefix is a
		// smaller code than its entry, so this ends.
		for code >= 256 {
			z.stack = append(z.stack, z.suffix[code])
			code = int(z.prefix[code])
		}
		z.finChar = z.suffix[code]
		z.stack = append(z.stack, z.finChar)
		z.incode = inCode
		return nil
	}
}

// newEntry is the end of zread's loop, run once the string is all out: it
// adds the new table entry and remembers the code.
func (z *reader) newEntry() {
	if code := z.freeEnt; code < z.maxMaxCode && z.oldCode != -1 {
		z.prefix[code] = uint16(z.oldCode)
		z.suffix[code] = z.finChar
		z.freeEnt = code + 1
	}
	z.oldCode = z.incode
}

// getCode reads one code. It returns io.EOF at the end of the data.
func (z *reader) getCode() (int, error) {
	if z.clearFlg || z.rOffset >= z.size || z.freeEnt > z.maxCode {
		// If the next entry will be too big for the current code size,
		// increase the size. This implies reading a new group too.
		if z.freeEnt > z.maxCode {
			z.nBits++
			if z.nBits == z.maxBits {
				z.maxCode = z.maxMaxCode
			} else {
				z.maxCode = maxCode(z.nBits)
			}
		}
		if z.clearFlg {
			z.nBits = initBits
			z.maxCode = maxCode(initBits)
			z.clearFlg = false
		}
		got, err := io.ReadFull(z.fp, z.gbuf[:z.nBits])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			// Deviation: the C's fread takes a read error for end of
			// data.
			return 0, err
		}
		if got <= 0 {
			return 0, io.EOF
		}
		z.rOffset = 0
		// Round size down to an integral number of codes. A group shorter
		// than one code leaves size at zero or below, and the C still
		// reads one code from it.
		z.size = got<<3 - (z.nBits - 1)
	}
	rOff := z.rOffset
	nb := z.nBits

	// Get to the first byte.
	bp := rOff >> 3
	rOff &= 7

	// Get first part (low order bits).
	code := int(z.gbuf[bp] >> rOff)
	bp++
	nb -= 8 - rOff
	rOff = 8 - rOff // now, offset into code

	// Get any 8 bit parts in the middle (at most one for up to 16 bits).
	if nb >= 8 {
		code |= int(z.gbuf[bp]) << rOff
		bp++
		rOff += 8
		nb -= 8
	}

	// High order bits. Deviation: when none are left the C still reads
	// gbuf[bp] and masks it to nothing, which is one past the buffer for
	// the last code of a 16-bit group.
	if nb > 0 {
		code |= int(z.gbuf[bp]&(1<<nb-1)) << rOff
	}
	z.rOffset += z.nBits
	return code, nil
}
