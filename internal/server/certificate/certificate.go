// Package certificate holds the TLS certificate the server presents and rereads
// it from disk when the certificate file changes, so a renewal does not need a
// restart.
package certificate

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type (
	// The Loader type holds a certificate pair and rereads it when the
	// certificate file on disk changes.
	Loader struct {
		logger      *slog.Logger
		certificate string
		key         string
		interval    time.Duration

		// The pair every handshake reads. Swapped whole on reload, so a
		// handshake never sees a certificate from one pair and a key from
		// another.
		current atomic.Pointer[tls.Certificate]
		// When the certificate file was last checked, in Unix nanoseconds.
		// Compare-and-swapped so that at most one handshake per interval pays
		// for the stat and the rest read the held pair.
		checked atomic.Int64
		mux     sync.Mutex
		// The certificate file's modification time when the held pair was
		// read, guarded by mux.
		modTime time.Time
	}

	// The Config type contains fields used to configure the Loader.
	Config struct {
		// The logger used to report reload outcomes.
		Logger *slog.Logger
		// The path to the PEM certificate file.
		Certificate string
		// The path to the PEM private key file, which must be readable only
		// by its owner.
		Key string
		// The shortest time between checks of the certificate file. Zero
		// means five seconds.
		Interval time.Duration
	}
)

var (
	// ErrKeyReadable is returned when the TLS key file can be read by someone
	// other than its owner.
	ErrKeyReadable = errors.New("tls key is readable by more than its owner")
)

// New returns a Loader holding the pair at the configured paths.
//
// The pair is read here rather than at the first handshake, so a path that does
// not hold a usable pair stops the server from starting instead of failing every
// connection.
func New(config Config) (*Loader, error) {
	if config.Interval <= 0 {
		config.Interval = 5 * time.Second
	}

	l := &Loader{
		logger:      config.Logger.With("component", "certificate"),
		certificate: config.Certificate,
		key:         config.Key,
		interval:    config.Interval,
	}

	pair, modTime, err := l.load()
	if err != nil {
		return nil, err
	}

	l.current.Store(pair)
	l.modTime = modTime
	l.checked.Store(time.Now().UnixNano())

	return l, nil
}

// GetCertificate returns the held pair, rereading it when the certificate file
// has changed. Its signature matches tls.Config.GetCertificate.
//
// A reload that fails keeps the pair already held and is retried on the next
// interval. The old pair is always a better answer than a failed handshake: a
// renewal writes the certificate and key one after the other, and a read that
// lands between them sees a pair that does not match.
func (l *Loader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	checked := l.checked.Load()

	now := time.Now().UnixNano()
	if now-checked < int64(l.interval) || !l.checked.CompareAndSwap(checked, now) {
		return l.current.Load(), nil
	}

	l.mux.Lock()
	defer l.mux.Unlock()

	info, err := os.Stat(l.certificate)
	if err != nil {
		l.logger.With("error", err, "path", l.certificate).Warn("failed to check tls certificate")
		return l.current.Load(), nil
	}

	// Equal rather than After, so a certificate restored from a backup with an
	// older modification time is still picked up.
	if info.ModTime().Equal(l.modTime) {
		return l.current.Load(), nil
	}

	pair, modTime, err := l.load()
	if err != nil {
		l.logger.With("error", err, "path", l.certificate).Warn("failed to reload tls certificate")
		return l.current.Load(), nil
	}

	l.current.Store(pair)
	l.modTime = modTime
	l.logger.With("path", l.certificate).Info("reloaded tls certificate")

	return pair, nil
}

// load reads the pair from disk and returns it with the certificate file's
// modification time.
//
// The time is taken before the read, so a write that lands between the two makes
// the recorded time stale and the next check read the pair again.
func (l *Loader) load() (*tls.Certificate, time.Time, error) {
	// The same refusal as the secret keyring: a key anyone else can read is
	// refused rather than narrowed, because whoever could read it already had
	// the chance.
	info, err := os.Stat(l.key)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to read tls key: %w", err)
	}

	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, time.Time{}, fmt.Errorf("%w: %s is %#o, want 0600", ErrKeyReadable, l.key, mode)
	}

	info, err = os.Stat(l.certificate)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to read tls certificate: %w", err)
	}

	modTime := info.ModTime()

	pair, err := tls.LoadX509KeyPair(l.certificate, l.key)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to load tls certificate pair: %w", err)
	}

	return &pair, modTime, nil
}
