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
	"time"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/pkg/manifest"
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
		// with the given name.
		Upsert(ctx context.Context, name string, value []byte, revision string) (database.Secret, error)
		// Get should return the secret with the given name, including its encrypted
		// value.
		Get(ctx context.Context, name string) (database.Secret, error)
		// List should return every secret, without their values.
		List(ctx context.Context) ([]database.Secret, error)
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
		cipher  Cipher
		rehash  func(ctx context.Context, workload string) error
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

	stored, err := s.secrets.Upsert(ctx, name, sealed, revision)
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

// Resolve returns env with every secret reference replaced by the value it names.
//
// This is the only thing that produces a secret's plaintext, and it exists for the
// reconciler to call as a workload starts. Returns ErrSecretNotFound naming the
// secret when a reference cannot be resolved: handing the workload the reference
// text would have it use that as the value.
func (s *SecretService) Resolve(ctx context.Context, env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return env, nil
	}

	// Read once each, however many variables reference the same secret.
	values := make(map[string]string)

	// Why a lookup came back empty, which the callback cannot report itself. A secret
	// nobody created and a key that cannot decrypt what it sealed both leave a
	// reference unresolved, and an operator told the wrong one goes looking in the
	// wrong place.
	var failed error
	var missing string

	resolved := make(map[string]string, len(env))
	for key, value := range env {
		expanded, err := manifest.Expand(value, func(reference manifest.Reference) (string, bool) {
			if reference.Kind != manifest.KindSecret {
				return "", false
			}

			name := reference.Name
			if value, ok := values[name]; ok {
				return value, true
			}

			stored, err := s.secrets.Get(ctx, name)
			switch {
			case errors.Is(err, database.ErrSecretNotFound):
				missing = name

				return "", false
			case err != nil:
				failed = fmt.Errorf("failed to load secret %s: %w", name, err)

				return "", false
			}

			opened, err := s.cipher.Open(name, stored.Value)
			if err != nil {
				failed = fmt.Errorf("failed to decrypt secret %s: %w", name, err)

				return "", false
			}

			values[name] = string(opened)

			return values[name], true
		})
		switch {
		case failed != nil:
			return nil, failed
		case missing != "":
			return nil, fmt.Errorf("%w: %s reads %s", ErrSecretNotFound, key, missing)
		case err != nil:
			return nil, fmt.Errorf("failed to resolve env %s: %w", key, err)
		}

		resolved[key] = expanded
	}

	return resolved, nil
}

// unchanged reports whether stored already holds value.
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
