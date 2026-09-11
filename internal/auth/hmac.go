package auth

import (
	"crypto/hmac"
	"hash"
)

// hmacNew is just a thin wrapper so the pbkdf2 helper reads cleanly.
// Returns a fresh HMAC keyed with `key` using the given hash factory.
func hmacNew(h func() hash.Hash, key []byte) hash.Hash {
	return hmac.New(h, key)
}
