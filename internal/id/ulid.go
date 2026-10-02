// Package id generates the identifiers used across Kumo: ULIDs for request
// IDs and prefixed resource IDs (i-, img-, key-, task-).
package id

import (
	"crypto/rand"
	"time"
)

// ULIDSize is the length of the string returned by ULID.
const ULIDSize = 26

// crockford is the Crockford Base32 alphabet used by the ULID spec.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ULID returns a 26 character ULID: a 48-bit big-endian millisecond timestamp
// followed by 80 bits of randomness, see https://github.com/ulid/spec.
// The result is lexicographically sortable by creation time, which is what
// makes ULIDs usable as request IDs and sort keys.
func ULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	// crypto/rand.Read never fails: since Go 1.24 it panics internally if the
	// system source is unavailable, so there is no error to handle here.
	_, _ = rand.Read(b[6:])

	return encodeULID(&b)
}

// encodeULID encodes 128 bits as 26 Crockford Base32 characters (130 bits,
// left padded with two zero bits).
func encodeULID(b *[16]byte) string {
	var out [ULIDSize]byte
	var acc uint16 // bit accumulator, at most 12 bits wide
	bits := uint8(0)
	idx := ULIDSize - 1

	for i := len(b) - 1; i >= 0; i-- {
		acc |= uint16(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[idx] = crockford[acc&0x1f]
			idx--
			acc >>= 5
			bits -= 5
		}
	}
	if idx >= 0 {
		out[idx] = crockford[acc&0x1f]
	}

	return string(out[:])
}
