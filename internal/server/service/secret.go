package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dsb-labs/orca/internal/server/database"
)

var (
	// ErrSecretNotFound is returned when the requested secret does not exist.
	ErrSecretNotFound = errors.New("secret not found")
	// ErrSecretInUse is returned when a secret a workload references is deleted
	// without being forced.
	ErrSecretInUse = errors.New("secret is in use")
	// ErrInvalidSecret is returned when a secret's name is not one orca will accept.
	ErrInvalidSecret = errors.New("invalid secret")
)

// How many random bytes a revision carries.
//
// Random rather than a counter: a secret deleted and re-created would restart a
// counter, so a workload reading it would hash the same as it did against the value
// that is gone and would keep running against a secret it no longer has.
const revisionLength = 16

// The names a secret or a variable may have, which are the names a workload may
// have. Either is referenced from a manifest and reported back to an operator, so
// both are held to the same shape.
var referenceNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type (
	// The SecretRepository interface describes the persistence operations the secret
	// service uses.
	SecretRepository interface {
		// Upsert should store the given encrypted value and revision as the secret
		// with the given name, recording which key sealed it.
		Upsert(ctx context.Context, name string, value []byte, revision, keyID string) (database.Secret, error)
		// Get should return the secret with the given name, including its encrypted
		// value.
		Get(ctx context.Context, name string) (database.Secret, error)
		// List should return every secret, without their values.
		List(ctx context.Context) ([]database.Secret, error)
		// ListSealed should return every secret including its value and the key
		// that sealed it.
		ListSealed(ctx context.Context) ([]database.Secret, error)
		// Rekey should replace every secret's sealed value with the one given for
		// it and record keyID as the key they are now sealed under, in one
		// transaction.
		Rekey(ctx context.Context, keyID string, sealed map[string][]byte) error
		// Delete should remove the secret with the given name.
		Delete(ctx context.Context, name string) error
		// UsedBy should name the workloads referencing the secret with the given
		// name.
		UsedBy(ctx context.Context, name string) ([]string, error)
	}

	// The Cipher interface describes how the service encrypts a secret's value.
	//
	// Declared here rather than taken as the concrete type so that the service can be
	// tested without a key on disk.
	Cipher interface {
		// Seal should encrypt value for the secret with the given name.
		Seal(name string, value []byte) ([]byte, error)
		// Open should decrypt a value sealed for the secret with the given name.
		Open(name string, sealed []byte) ([]byte, error)
	}

	// The Secret type describes a secret as it is reported to a caller.
	//
	// There is no value on it, on purpose. A secret is not readable back: once set,
	// the only thing that sees the plaintext is a workload being started.
	Secret struct {
		// The name that identifies the secret.
		Name string
		// Changes whenever the secret's value changes, and never otherwise.
		//
		// Reported so that an operator can confirm a rotation landed. It says nothing
		// about the value, being random rather than derived from it.
		Revision string
		// The names of the workloads referencing the secret.
		UsedBy []string
		// The time the secret was created.
		CreatedAt time.Time
		// The time the secret's value last changed.
		UpdatedAt time.Time
	}

	// The SecretService type owns secrets and the encryption they are stored under.
	SecretService struct {
		logger  *slog.Logger
		secrets SecretRepository
		rehash  func(ctx context.Context, workload string) error

		// Guards the cipher and the identifier naming it, which move together and
		// only when a rekey replaces both.
		//
		// Every read of a sealed value is held for the whole of the read, not only
		// across the decryption: the row and the cipher have to come from the same
		// side of a rekey. A caller that read a row under the old key and decrypted
		// it under the new one would see a value that will not open, in a database
		// where nothing is wrong.
		mu     sync.RWMutex
		cipher Cipher
		keyID  string
	}

	// The SecretServiceConfig type contains fields used to construct a
	// SecretService.
	SecretServiceConfig struct {
		// The logger used for service events.
		Logger *slog.Logger
		// The repository holding the secrets.
		Secrets SecretRepository
		// The cipher a secret's value is stored under.
		Cipher Cipher
		// The identifier of the key the cipher holds, recorded against every
		// secret the service seals so that the database says which key opens it.
		KeyID string
		// Called for each workload referencing a secret whose value changed, so that
		// its specification hash moves and the reconciler replaces its instances. May
		// be nil, in which case a rotation does not redeploy anything.
		Rehash func(ctx context.Context, workload string) error
	}
)

// NewSecretService returns a new instance of the SecretService type.
func NewSecretService(config SecretServiceConfig) *SecretService {
	return &SecretService{
		logger:  config.Logger.With("component", "service"),
		secrets: config.Secrets,
		cipher:  config.Cipher,
		keyID:   config.KeyID,
		rehash:  config.Rehash,
	}
}

// Set stores value as the secret with the given name, returning the stored secret
// and whether it was newly created.
//
// Setting a secret to the value it already holds is a no-op: the revision stays put,
// so nothing reading it is redeployed. That mirrors applying an unchanged manifest,
// and it means a configuration management tool that sets every secret on every run
// does not restart the fleet each time.
//
// A value that did change moves the revision, and every workload referencing the
// secret is rehashed so the reconciler replaces its instances.
func (s *SecretService) Set(ctx context.Context, name string, value []byte) (Secret, bool, error) {
	if !referenceNamePattern.MatchString(name) || len(name) > 63 {
		return Secret{}, false, fmt.Errorf("%w: name must be lowercase alphanumeric, optionally separated by dashes", ErrInvalidSecret)
	}

	// Held across the comparison, the sealing and the write, so a value cannot be
	// sealed under one key and recorded against another. A rekey running at the same
	// time waits for this rather than overtaking it.
	s.mu.RLock()
	defer s.mu.RUnlock()

	existing, err := s.secrets.Get(ctx, name)
	switch {
	case err != nil && !errors.Is(err, database.ErrSecretNotFound):
		return Secret{}, false, fmt.Errorf("failed to load secret: %w", err)
	case err == nil && s.unchanged(name, existing.Value, value):
		secret, err := s.hydrate(ctx, existing)

		return secret, false, err
	}

	created := errors.Is(err, database.ErrSecretNotFound)

	sealed, err := s.cipher.Seal(name, value)
	if err != nil {
		return Secret{}, false, fmt.Errorf("failed to encrypt secret: %w", err)
	}

	revision, err := newRevision()
	if err != nil {
		return Secret{}, false, err
	}

	stored, err := s.secrets.Upsert(ctx, name, sealed, revision, s.keyID)
	if err != nil {
		return Secret{}, false, err
	}

	// The workloads reading it are rehashed after the value has landed, so nothing is
	// redeployed to pick up a value that failed to store.
	if err = s.redeploy(ctx, name); err != nil {
		return Secret{}, false, err
	}

	s.logger.With("secret", name, "created", created).Info("secret set")

	secret, err := s.hydrate(ctx, stored)

	return secret, created, err
}

// Get returns the secret with the given name, along with the workloads referencing
// it. The value is not part of what is returned.
func (s *SecretService) Get(ctx context.Context, name string) (Secret, error) {
	stored, err := s.secrets.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrSecretNotFound):
		return Secret{}, fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	case err != nil:
		return Secret{}, err
	}

	return s.hydrate(ctx, stored)
}

// List returns every secret, along with the workloads referencing each one. No
// value is part of what is returned.
func (s *SecretService) List(ctx context.Context) ([]Secret, error) {
	stored, err := s.secrets.List(ctx)
	if err != nil {
		return nil, err
	}

	secrets := make([]Secret, 0, len(stored))
	for _, row := range stored {
		secret, err := s.hydrate(ctx, row)
		if err != nil {
			return nil, err
		}

		secrets = append(secrets, secret)
	}

	return secrets, nil
}

// Delete removes the secret with the given name.
//
// A secret a workload references is refused unless force is set, and the error names
// the workloads reading it. Forcing it through leaves those workloads running: they
// find out at their next start, which is when the value is actually needed.
func (s *SecretService) Delete(ctx context.Context, name string, force bool) error {
	usedBy, err := s.secrets.UsedBy(ctx, name)
	if err != nil {
		return err
	}

	if len(usedBy) > 0 && !force {
		return fmt.Errorf("%w: read by %s", ErrSecretInUse, strings.Join(usedBy, ", "))
	}

	err = s.secrets.Delete(ctx, name)
	switch {
	case errors.Is(err, database.ErrSecretNotFound):
		return fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	case err != nil:
		return err
	}

	// The workloads that referenced it are rehashed for the same reason a rotation
	// rehashes them: what they were started against no longer describes what orca
	// holds, and the hash is how that is reported.
	if err = s.redeploy(ctx, name); err != nil {
		return err
	}

	s.logger.With("secret", name, "forced", force).Info("secret deleted")

	return nil
}

// Value returns the plaintext of the named secret, for the resolver to substitute
// into a workload's environment as it starts.
//
// This and Resolve are the only things that produce a secret's plaintext. Reports
// ErrSecretNotFound when nothing holds the name, which the resolver turns into a
// refusal to start rather than handing the workload the reference text.
func (s *SecretService) Value(ctx context.Context, name string) (string, error) {
	// The row and the cipher have to come from the same side of a rekey, so the read
	// is held for both rather than only for the decryption.
	s.mu.RLock()
	defer s.mu.RUnlock()

	stored, err := s.secrets.Get(ctx, name)
	switch {
	case errors.Is(err, database.ErrSecretNotFound):
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	case err != nil:
		return "", fmt.Errorf("failed to load secret %s: %w", name, err)
	}

	opened, err := s.cipher.Open(name, stored.Value)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt secret %s: %w", name, err)
	}

	return string(opened), nil
}

// unchanged reports whether stored already holds value.
//
// Called with the cipher's read lock already held, and so does not take it. Taking
// it twice on one goroutine deadlocks whenever a rekey is waiting between the two,
// because a pending writer stops further readers.
//
// A stored value that will not open counts as changed. That is what a key rotated
// out from under the database looks like, and re-sealing under the current key is
// more useful than refusing to move.
func (s *SecretService) unchanged(name string, stored, value []byte) bool {
	opened, err := s.cipher.Open(name, stored)
	if err != nil {
		return false
	}

	return string(opened) == string(value)
}

// redeploy moves the specification hash of every workload referencing the named
// secret, so that the reconciler replaces the instances reading the old value.
func (s *SecretService) redeploy(ctx context.Context, name string) error {
	if s.rehash == nil {
		return nil
	}

	usedBy, err := s.secrets.UsedBy(ctx, name)
	if err != nil {
		return err
	}

	for _, workload := range usedBy {
		if err = s.rehash(ctx, workload); err != nil {
			return fmt.Errorf("failed to redeploy workload %s: %w", workload, err)
		}
	}

	return nil
}

func (s *SecretService) hydrate(ctx context.Context, row database.Secret) (Secret, error) {
	usedBy, err := s.secrets.UsedBy(ctx, row.Name)
	if err != nil {
		return Secret{}, err
	}

	return Secret{
		Name:      row.Name,
		Revision:  row.Revision,
		UsedBy:    usedBy,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

// newRevision returns the value a secret's revision moves to when it changes.
func newRevision() (string, error) {
	buf := make([]byte, revisionLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate secret revision: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// Rekey re-encrypts every secret under the given cipher and records keyID as the key
// they are sealed under, returning how many were moved and the key they were sealed
// under before.
//
// The cipher the service seals with is replaced only once the rewrite has committed,
// under the same lock that holds every read of a sealed value. A caller reading a
// secret while this runs therefore sees the values and the cipher from the same side
// of the rekey, never a mixture of the two.
//
// Every value is decrypted and re-encrypted before anything is written, and a value
// that will not open aborts the whole thing. Writing past one would leave a secret
// nothing can read, recorded as though it had moved.
//
// No revision changes, so nothing referencing a secret is redeployed by this. A
// rekey changes how a value is stored, not what it is.
func (s *SecretService) Rekey(ctx context.Context, cipher Cipher, keyID string) (int, string, error) {
	// The write lock, so no value is sealed under the outgoing key while the rewrite
	// is in flight and none is read between the commit and the swap.
	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.keyID

	stored, err := s.secrets.ListSealed(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("failed to load secrets: %w", err)
	}

	sealed := make(map[string][]byte, len(stored))

	for _, secret := range stored {
		opened, err := s.cipher.Open(secret.Name, secret.Value)
		if err != nil {
			// Named without its value, and counted without its name elsewhere. That
			// a particular secret will not open is what an operator needs; what is
			// in it is not.
			return 0, "", fmt.Errorf("failed to decrypt secret %s: %w", secret.Name, err)
		}

		resealed, err := cipher.Seal(secret.Name, opened)

		// Zeroed as soon as it has been resealed rather than left for the collector.
		// Every plaintext orca holds is in this loop, which is the largest number of
		// them that are ever in memory at once.
		clear(opened)

		if err != nil {
			return 0, "", fmt.Errorf("failed to encrypt secret %s: %w", secret.Name, err)
		}

		sealed[secret.Name] = resealed
	}

	if err = s.secrets.Rekey(ctx, keyID, sealed); err != nil {
		return 0, "", err
	}

	// After the commit, so a rewrite that failed leaves the service sealing and
	// opening under the key the database still names.
	s.cipher = cipher
	s.keyID = keyID

	s.logger.With("secrets", len(sealed), "key", keyID).Info("secrets re-encrypted under a new key")

	return len(sealed), previous, nil
}
