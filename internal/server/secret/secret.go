// Package secret provides the encryption orca stores a secret's value under.
//
// A value is sealed on its way into the database and opened only to hand it to a
// workload that is starting. Nothing else opens one: a secret is not readable back
// through the API, so the ciphertext in the database is the only copy orca keeps.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	keyInfo = "orca secret encryption v1"
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

// LoadKey reads the key at path, generating one if nothing is there yet.
//
// A generated key is written before it is used, so a server that came up once and
// sealed something can open it again. The file is readable only by the user running
// the server, like the database beside it: anything that can read the key can read
// every secret orca holds.
//
// An existing key that anyone else can read is refused rather than narrowed. Whoever
// could read it has already had the chance, so tightening the mode would hide that
// rather than undo it, and orca cannot tell a mistake from a deliberate share.
func LoadKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(key) != KeyLength {
			return nil, fmt.Errorf("%w: %s holds %d bytes, need %d", ErrInvalidKey, path, len(key), KeyLength)
		}

		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read encryption key: %w", err)
		}

		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			return nil, fmt.Errorf("%w: %s is %#o, want 0600", ErrKeyReadable, path, mode)
		}

		return key, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("failed to read encryption key: %w", err)
	}

	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create encryption key directory: %w", err)
	}

	key = make([]byte, KeyLength)
	if _, err = rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate encryption key: %w", err)
	}

	// Exclusive, so that two servers racing to create it cannot each write a key and
	// leave one of them unable to open what it sealed.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadKey(path)
		}

		return nil, fmt.Errorf("failed to create encryption key: %w", err)
	}
	defer f.Close()

	if _, err = f.Write(key); err != nil {
		return nil, fmt.Errorf("failed to write encryption key: %w", err)
	}

	return key, nil
}
