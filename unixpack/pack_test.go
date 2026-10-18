// Copyright 2007 Sun Microsystems, Inc.  All rights reserved.
// Copyright (c) 1984, 1986, 1987, 1988, 1989 AT&T  All Rights Reserved
// SPDX-License-Identifier: CDDL-1.0
//
// Ported from illumos usr/src/cmd/pack/pack.c (illumos-gate 6c2516d0a0ac);
// see THIRD_PARTY_NOTICES. Test-only: it generates the fixtures under -update.

package unixpack

import "errors"

const packEND = 256

// errTrivial is pack(1) refusing an empty file or one of a single byte
// value.
var errTrivial = errors.New("pack: zero length or trivial file")

// pack encodes in as pack(1) does with -f: packfile, input, heapify and
// output, minus file handling, statistics and the no-saving refusal.
func pack(in []byte) ([]byte, error) {
	if len(in) == 0 { // main: "cannot pack a zero length file"
		return nil, errTrivial
	}
	var (
		count    [packEND + 1]int64
		parent   [2*packEND + 1]int
		length   [packEND + 1]int
		bits     [packEND + 1]int64
		levcount [25]int
		heap     [packEND + 2]struct {
			count int64
			node  int
		}
		n int
	)
	heapify := func(i int) {
		heapsubi := heap[i]
		lastparent := n / 2
		for i <= lastparent {
			k := 2 * i
			if heap[k].count > heap[k+1].count && k < n {
				k++
			}
			if heapsubi.count < heap[k].count {
				break
			}
			heap[i] = heap[k]
			i = k
		}
		heap[i] = heapsubi
	}

	// input: gather character frequency statistics
	for _, b := range in {
		count[b] += 2
	}

	// packfile: put occurring chars in heap with their counts
	diffbytes := -1
	count[packEND] = 1
	var insize int64
	for i := packEND; i >= 0; i-- {
		parent[i] = 0
		if count[i] > 0 {
			diffbytes++
			insize += count[i]
			n++
			heap[n].count = count[i]
			heap[n].node = i
		}
	}
	if diffbytes == 1 {
		return nil, errTrivial
	}
	insize >>= 1
	for i := n / 2; i >= 1; i-- {
		heapify(i)
	}

	// build Huffman tree
	lastnode := packEND
	for n > 1 {
		lastnode++
		parent[heap[1].node] = lastnode
		inc := heap[1].count
		heap[1] = heap[n]
		n--
		heapify(1)
		parent[heap[1].node] = lastnode
		heap[1].node = lastnode
		heap[1].count += inc
		heapify(1)
	}
	parent[lastnode] = 0

	// assign lengths to encoding for each character
	maxlev := 0
	for i := 0; i <= packEND; i++ {
		c := 0
		for p := parent[i]; p != 0; p = parent[p] {
			c++
		}
		if c > 24 { // checked after the loop in the C, which indexes past levcount first
			return nil, errors.New("pack: Huffman tree has too many levels")
		}
		levcount[c]++
		length[i] = c
		maxlev = max(maxlev, c)
	}

	// compute bit patterns for each character
	inc := int64(1) << 24
	inc >>= maxlev
	var mask int64
	for i := maxlev; i > 0; i-- {
		for c := 0; c <= packEND; c++ {
			if length[c] == i {
				bits[c] = mask
				mask += inc
			}
		}
		mask &^= inc
		inc <<= 1
	}

	// output: the header, the length and the dictionary
	out := []byte{037, 036, byte(insize >> 24), byte(insize >> 16), byte(insize >> 8), byte(insize), byte(maxlev)}
	for i := 1; i < maxlev; i++ {
		out = append(out, byte(levcount[i]))
	}
	out = append(out, byte(levcount[maxlev]-2))
	for i := 1; i <= maxlev; i++ {
		for c := 0; c < packEND; c++ {
			if length[c] == i {
				out = append(out, byte(c))
			}
		}
	}

	// output the text. The C shifts a 24-bit code into a long and copies
	// its four low bytes out from the most significant; the first ORs
	// into the byte in progress.
	bitsleft := 8
	for i := 0; ; i++ {
		c := packEND
		if i < len(in) {
			c = int(in[i])
		}
		m := uint32(bits[c] << bitsleft)
		shuf := [4]byte{byte(m >> 24), byte(m >> 16), byte(m >> 8), byte(m)}
		if bitsleft == 8 {
			out = append(out, shuf[0])
		} else {
			out[len(out)-1] |= shuf[0]
		}
		q := 1
		bitsleft -= length[c]
		for bitsleft < 0 {
			out = append(out, shuf[q])
			q++
			bitsleft += 8
		}
		if c == packEND {
			return out, nil
		}
	}
}
