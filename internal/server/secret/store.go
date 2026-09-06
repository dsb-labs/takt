package secret

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/xid"
)

var (
	// ErrKeyNotFound is returned when the keyring holds no key with the given
	// identifier.
	ErrKeyNotFound = errors.New("encryption key not found")
)

// The extension every key file in a keyring carries.
const keyExtension = ".key"

// The Store type is a directory of encryption keys, each addressed by an
// identifier.
//
// A key is addressed rather than fixed at a path so that rotating one is a write
// followed by a decision, instead of a write over the key still in use. The
// database records which identifier its secrets are sealed under, so which file is
// current is something the database answers and the directory does not have to.
type Store struct {
	directory string
}

// NewStore returns a Store over the keys in the given directory, creating it if
// nothing is there.
//
// The directory is readable only by the user running the server. Anything that can
// read it can read every secret takt holds.
func NewStore(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create keyring directory: %w", err)
	}

	return &Store{directory: directory}, nil
}

// Directory returns where the keyring's keys are kept.
func (s *Store) Directory() string {
	return s.directory
}

// Create generates a key, writes it to the keyring, and returns its identifier.
//
// The key is on disk before the caller is told about it, so a server that recorded
// an identifier can always find the key behind it. A key nothing goes on to
// reference is inert: it seals nothing, and it opens nothing.
func (s *Store) Create() (string, error) {
	key := make([]byte, KeyLength)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("failed to generate encryption key: %w", err)
	}

	id := xid.New().String()

	// Exclusive, so a key is never written over one that already answers to the same
	// identifier. Nothing should collide, and a collision that did would leave
	// whatever was sealed under the first key unopenable.
	f, err := os.OpenFile(s.path(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("failed to create encryption key: %w", err)
	}
	defer f.Close()

	if _, err = f.Write(key); err != nil {
		return "", fmt.Errorf("failed to write encryption key: %w", err)
	}

	// The bytes are on disk before the identifier is returned, so a crash after this
	// point leaves a key that can still be read rather than a name with nothing
	// behind it.
	if err = f.Sync(); err != nil {
		return "", fmt.Errorf("failed to flush encryption key: %w", err)
	}

	return id, nil
}

// Read returns the key with the given identifier.
//
// A key anyone other than its owner can read is refused rather than narrowed.
// Whoever could read it has already had the chance, so tightening the mode would
// hide that rather than undo it, and takt cannot tell a mistake from a deliberate
// share.
func (s *Store) Read(id string) ([]byte, error) {
	path := s.path(id)

	key, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, id)
	case err != nil:
		return nil, fmt.Errorf("failed to read encryption key: %w", err)
	}

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
}

// List returns the identifier of every key in the keyring.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, fmt.Errorf("failed to read keyring directory: %w", err)
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), keyExtension) {
			continue
		}

		ids = append(ids, strings.TrimSuffix(entry.Name(), keyExtension))
	}

	return ids, nil
}

// Remove the key with the given identifier. Removing one that is not there is not
// an error, so a sweep does not have to race whatever else is removing keys.
func (s *Store) Remove(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove encryption key: %w", err)
	}

	return nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.directory, id+keyExtension)
}
