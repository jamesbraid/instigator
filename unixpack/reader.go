// Copyright (c) 2009 Xin LI <delphij@FreeBSD.org>
// SPDX-License-Identifier: BSD-2-Clause
//
// Ported from FreeBSD usr.bin/gzip/unpack.c (freebsd-src 42b3884); see
// THIRD_PARTY_NOTICES.

// Package unixpack reads System V pack(1) data, the ".z" format: a static
// Huffman code stored as per-level leaf counts and a symbol table, then the
// coded bytes and an end-of-block symbol.
//
// It accepts exactly what FreeBSD gzip accepts, including an end-of-block
// symbol before the stored length, which writes a byte and decoding goes
// on. It differs only where Go forces it: errors wrap ErrCorrupt instead of
// exiting, read errors are returned rather than taken for end of data, a
// short header is an error rather than left uninitialised, and the byte the
// C reads past its symbol table on an early end-of-block is a zero here.
package unixpack

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// Magic is the two-byte signature that starts every pack(1) file.
const Magic = "\x1f\x1e"

// ErrCorrupt is returned (possibly wrapped) when the data is not valid
// pack(1) output. Test for it with errors.Is.
var ErrCorrupt = errors.New("unixpack: corrupt data")

const (
	packHeaderLength = 7  // PACK_HEADER_LENGTH
	htreeMaxLevel    = 24 // HTREE_MAXLEVEL
)

// IsPacked reports whether b begins with the pack(1) magic bytes.
func IsPacked(b []byte) bool {
	return len(b) >= len(Magic) && b[0] == Magic[0] && b[1] == Magic[1]
}

// reader holds the C's unpack descriptor and the state of unpack_decode.
type reader struct {
	fpIn *bufio.Reader
	err  error // sticky

	treeLevels int                // deepest level, counting from 0
	symbolsIn  [htreeMaxLevel]int // leaves at each level
	inodesIn   [htreeMaxLevel]int // internal nodes at each level
	symbol     []byte             // the symbol table
	symbolEOB  int                // index of the EOB symbol
	tree       [htreeMaxLevel]int // index of each level's first symbol

	uncompressedSize int64
	bytesOut         int64

	thisLevel, thisCode int
	thisByte            byte
	bitsLeft            int // bits of thisByte not yet used
}

// NewReader returns a reader that unpacks pack(1) data from r. It reads the
// header and symbol table immediately and returns an error wrapping
// ErrCorrupt if they are missing or invalid. Closing the result does not
// close r.
func NewReader(r io.Reader) (io.ReadCloser, error) {
	z := &reader{fpIn: bufio.NewReaderSize(r, 64<<10)}
	var hdr [packHeaderLength]byte
	if _, err := io.ReadFull(z.fpIn, hdr[:]); err != nil {
		// Deviation: the C reads what it can and leaves the rest of
		// the header uninitialised.
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("%w: short header", ErrCorrupt)
		}
		return nil, err
	}
	// gzip.c checks the signature before calling unpack.
	if !IsPacked(hdr[:]) {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}

	// Obtain uncompressed length (bytes 2, 3, 4, 5).
	for i := 2; i <= 5; i++ {
		z.uncompressedSize = z.uncompressedSize<<8 | int64(hdr[i])
	}

	// Get the levels of the tree.
	z.treeLevels = int(hdr[6])
	if z.treeLevels > htreeMaxLevel || z.treeLevels < 1 {
		return nil, fmt.Errorf("%w: Huffman tree has insane levels", ErrCorrupt)
	}
	// We count from 0, so adjust to match the array upper bound.
	z.treeLevels--

	// Read the levels' symbol count table and calculate the total.
	symbolSize := 1 // EOB
	for i := 0; i <= z.treeLevels; i++ {
		b, err := z.fpIn.ReadByte()
		if err != nil {
			return nil, truncated(err, "file appears to be truncated")
		}
		z.symbolsIn[i] = int(b)
		symbolSize += z.symbolsIn[i]
	}
	if symbolSize > 256 {
		return nil, fmt.Errorf("%w: bad symbol table", ErrCorrupt)
	}

	// Deviation: a spare zero byte in the EOB slot, which the C reads past
	// the end of its table when EOB arrives early. The level arithmetic
	// keeps every other index inside the table.
	z.symbol = make([]byte, symbolSize+1)

	// Read in the symbol table. pack(1) stores the last level's count two
	// short so that it fits in a byte; one of the two is EOB, which is not
	// transmitted.
	z.symbolsIn[z.treeLevels]++
	for i := 0; i <= z.treeLevels; i++ {
		z.tree[i] = z.symbolEOB
		for j := 0; j < z.symbolsIn[i]; j++ {
			b, err := z.fpIn.ReadByte()
			if err != nil {
				return nil, truncated(err, "symbol table truncated")
			}
			z.symbol[z.symbolEOB] = b
			z.symbolEOB++
		}
	}

	// Now take account of the EOB symbol as well.
	z.symbolsIn[z.treeLevels]++

	// Internal nodes at a level are half of everything one level down;
	// the last level has none.
	for level := z.treeLevels - 1; level >= 0; level-- {
		z.inodesIn[level] = (z.inodesIn[level+1] + z.symbolsIn[level+1]) / 2
	}
	return z, nil
}

func truncated(err error, what string) error {
	if err == io.EOF {
		return fmt.Errorf("%w: %s", ErrCorrupt, what)
	}
	return err
}

func (z *reader) Close() error { return nil }

func (z *reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && z.err == nil {
		var b byte
		b, z.err = z.decode()
		if z.err == nil {
			p[n] = b
			n++
		}
	}
	if n > 0 {
		return n, nil
	}
	return 0, z.err
}

// decode is one step of unpack_decode's loop: it feeds bits into thisCode
// until they name a leaf, and returns that leaf's symbol. It returns io.EOF
// when the data is finished.
func (z *reader) decode() (byte, error) {
	for {
		if z.bitsLeft == 0 {
			b, err := z.fpIn.ReadByte()
			if err == io.EOF {
				if z.bytesOut != z.uncompressedSize {
					return 0, fmt.Errorf("%w: premature EOF", ErrCorrupt)
				}
				return 0, io.EOF
			}
			if err != nil {
				// Deviation: the C's fgetc takes a read error for end
				// of data.
				return 0, err
			}
			z.thisByte, z.bitsLeft = b, 8
		}
		z.bitsLeft--
		z.thisCode = z.thisCode<<1 | int(z.thisByte>>z.bitsLeft)&1

		// Did we get a symbol (a leaf node)?
		if z.thisCode >= z.inodesIn[z.thisLevel] {
			inLevelIndex := z.thisCode - z.inodesIn[z.thisLevel]
			if inLevelIndex > z.symbolsIn[z.thisLevel] {
				return 0, fmt.Errorf("%w: file corrupt", ErrCorrupt)
			}
			thisSymbol := z.tree[z.thisLevel] + inLevelIndex
			if thisSymbol == z.symbolEOB && z.bytesOut == z.uncompressedSize {
				return 0, io.EOF
			}
			z.bytesOut++
			// Prepare for the next input.
			z.thisLevel, z.thisCode = 0, 0
			return z.symbol[thisSymbol], nil
		}
		z.thisLevel++
		if z.thisLevel > z.treeLevels {
			return 0, fmt.Errorf("%w: file corrupt", ErrCorrupt)
		}
	}
}
