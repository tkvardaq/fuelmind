// Package auth — PBKDF2 helper.
//
// stdlib crypto/pbkdf2 was added in Go 1.24. We target Go 1.23+ in
// this repo, so we vendor the PBKDF2 implementation here, copied
// from the stdlib (released under BSD-3-Clause) so the function
// works on both 1.23 and 1.24+. When we drop 1.23 support we can
// remove this file and call crypto/pbkdf2.Key directly.
package auth

import (
	"hash"
)

// pbkdf2Key is a minimal, direct port of the stdlib crypto/pbkdf2
// implementation. It supports only the HMAC-SHA256 path we use.
// See: https://datatracker.ietf.org/doc/html/rfc2898#section-5.2
func pbkdf2Key(password, salt []byte, iter, keyLen int, h func() hash.Hash) []byte {
	prf := hmacNew(h, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	dk := make([]byte, 0, numBlocks*hashLen)
	sLen := len(salt)
	buf := make([]byte, sLen+4)
	copy(buf, salt)

	for block := 1; block <= numBlocks; block++ {
		buf[sLen+0] = byte(block >> 24)
		buf[sLen+1] = byte(block >> 16)
		buf[sLen+2] = byte(block >> 8)
		buf[sLen+3] = byte(block)

		// U_1 = PRF(P, S || INT(i)) — copy out so we own the buffer.
		u := make([]byte, hashLen)
		prf.Reset()
		prf.Write(buf)
		u = prf.Sum(u[:0])

		t := make([]byte, hashLen)
		copy(t, u)

		// U_n = PRF(P, U_{n-1}); T = U_1 XOR U_2 XOR ... XOR U_iter
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for x := range t {
				t[x] ^= u[x]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}
