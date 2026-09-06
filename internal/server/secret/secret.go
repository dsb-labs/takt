// Package secret provides the encryption takt stores a secret's value under.
//
// A value is sealed on its way into the database and opened only to hand it to a
// workload that is starting. Nothing else opens one: a secret is not readable back
// through the API, so the ciphertext in the database is the only copy takt keeps.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

var (
	// ErrInvalidKey is returned when a key is not the length the cipher needs.
	ErrInvalidKey = errors.New("invalid encryption key")
	// ErrKeyReadable is returned when a key file can be read by someone other than
	// its owner.
	ErrKeyReadable = errors.New("encryption key is readable by more than its owner")
	// ErrInvalidCiphertext is returned when a sealed value cannot be opened, which
	// covers a value that was tampered with, one sealed under a different key, and
	// one sealed under a different name.
	ErrInvalidCiphertext = errors.New("invalid ciphertext")
)

const (
	// KeyLength is the number of bytes a key file holds, which is what AES-256
	// needs.
	KeyLength = 32

	// What the file key is expanded with before it becomes the encryption key. It
	// names this use of the key, so the same file can serve another purpose later
	// without one use weakening the other.
	keyInfo = "takt secret encryption v1"
)

// The Cipher type seals and opens a secret's value.
type Cipher struct {
	aead cipher.AEAD
}

// New returns a Cipher that seals values under the given key, which must be
// KeyLength bytes.
//
// The key is expanded rather than used directly, so that what is on disk is keying
// material rather than the encryption key itself.
func New(key []byte) (*Cipher, error) {
	if len(key) != KeyLength {
		return nil, fmt.Errorf("%w: need %d bytes, got %d", ErrInvalidKey, KeyLength, len(key))
	}

	derived, err := hkdf.Key(sha256.New, key, nil, keyInfo, KeyLength)
	if err != nil {
		return nil, fmt.Errorf("failed to derive encryption key: %w", err)
	}

	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("failed to construct cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to construct cipher mode: %w", err)
	}

	return &Cipher{aead: aead}, nil
}

// Seal encrypts value for the secret with the given name.
//
// The name is authenticated along with the value, so a ciphertext moved to another
// row fails to open rather than decrypting as whichever secret now holds it. The
// nonce is random per call and prepended to the result, so sealing the same value
// twice produces different bytes.
func (c *Cipher) Seal(name string, value []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	return c.aead.Seal(nonce, nonce, value, []byte(name)), nil
}

// Open decrypts a value sealed for the secret with the given name.
//
// Returns ErrInvalidCiphertext when the value does not open, which does not
// distinguish between the reasons it might not: a caller that could tell tampering
// from the wrong key from the wrong name would be told something about the key.
func (c *Cipher) Open(name string, sealed []byte) ([]byte, error) {
	size := c.aead.NonceSize()
	if len(sealed) < size {
		return nil, fmt.Errorf("%w: shorter than a nonce", ErrInvalidCiphertext)
	}

	value, err := c.aead.Open(nil, sealed[:size], sealed[size:], []byte(name))
	if err != nil {
		return nil, ErrInvalidCiphertext
	}

	return value, nil
}
