package certificate_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/server/certificate"
)

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("holds a valid pair", func(t *testing.T) {
		cert, key := pairPaths(t)
		serial := generatePair(t, cert, key)

		loader, err := certificate.New(certificate.Config{
			Logger:      newTestLogger(t),
			Certificate: cert,
			Key:         key,
		})
		require.NoError(t, err)

		held, err := loader.GetCertificate(nil)
		require.NoError(t, err)
		assert.Equal(t, serial.String(), serialOf(t, held).String())
	})

	t.Run("refuses a readable key", func(t *testing.T) {
		cert, key := pairPaths(t)
		generatePair(t, cert, key)
		require.NoError(t, os.Chmod(key, 0o644))

		_, err := certificate.New(certificate.Config{
			Logger:      newTestLogger(t),
			Certificate: cert,
			Key:         key,
		})
		assert.ErrorIs(t, err, certificate.ErrKeyReadable)
	})

	t.Run("refuses a mismatched pair", func(t *testing.T) {
		cert, key := pairPaths(t)
		generatePair(t, cert, key)

		otherCert, otherKey := pairPaths(t)
		generatePair(t, otherCert, otherKey)

		_, err := certificate.New(certificate.Config{
			Logger:      newTestLogger(t),
			Certificate: cert,
			Key:         otherKey,
		})
		assert.Error(t, err)
	})

	t.Run("refuses a missing file", func(t *testing.T) {
		cert, key := pairPaths(t)
		generatePair(t, cert, key)

		_, err := certificate.New(certificate.Config{
			Logger:      newTestLogger(t),
			Certificate: filepath.Join(t.TempDir(), "missing.pem"),
			Key:         key,
		})
		assert.Error(t, err)
	})
}

func TestLoader_GetCertificate(t *testing.T) {
	t.Parallel()

	t.Run("reloads when the certificate file changes", func(t *testing.T) {
		cert, key := pairPaths(t)
		generatePair(t, cert, key)

		loader, err := certificate.New(certificate.Config{
			Logger:      newTestLogger(t),
			Certificate: cert,
			Key:         key,
			Interval:    time.Nanosecond,
		})
		require.NoError(t, err)

		serial := generatePair(t, cert, key)
		touch(t, cert)

		held, err := loader.GetCertificate(nil)
		require.NoError(t, err)
		assert.Equal(t, serial.String(), serialOf(t, held).String())
	})

	t.Run("keeps the held pair when a reload fails", func(t *testing.T) {
		cert, key := pairPaths(t)
		serial := generatePair(t, cert, key)

		loader, err := certificate.New(certificate.Config{
			Logger:      newTestLogger(t),
			Certificate: cert,
			Key:         key,
			Interval:    time.Nanosecond,
		})
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(cert, []byte("not a certificate"), 0o600))
		touch(t, cert)

		held, err := loader.GetCertificate(nil)
		require.NoError(t, err)
		assert.Equal(t, serial.String(), serialOf(t, held).String())
	})

	t.Run("does not check within the interval", func(t *testing.T) {
		cert, key := pairPaths(t)
		serial := generatePair(t, cert, key)

		loader, err := certificate.New(certificate.Config{
			Logger:      newTestLogger(t),
			Certificate: cert,
			Key:         key,
			Interval:    time.Hour,
		})
		require.NoError(t, err)

		generatePair(t, cert, key)
		touch(t, cert)

		held, err := loader.GetCertificate(nil)
		require.NoError(t, err)
		assert.Equal(t, serial.String(), serialOf(t, held).String())
	})
}

// pairPaths returns paths for a certificate pair in a fresh temporary
// directory.
func pairPaths(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()

	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

// generatePair writes a self-signed certificate pair to the given paths and
// returns the certificate's serial number.
func generatePair(t *testing.T, cert, key string) *big.Int {
	t.Helper()

	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "takt test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(private)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	require.NoError(t, os.WriteFile(cert, certPEM, 0o600))
	require.NoError(t, os.WriteFile(key, keyPEM, 0o600))

	return serial
}

// touch moves the file's modification time forward far enough to defeat coarse
// filesystem timestamps.
func touch(t *testing.T, path string) {
	t.Helper()

	when := time.Now().Add(10 * time.Second)
	require.NoError(t, os.Chtimes(path, when, when))
}

// serialOf returns the serial number of the pair's leaf certificate.
func serialOf(t *testing.T, pair *tls.Certificate) *big.Int {
	t.Helper()

	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	require.NoError(t, err)

	return leaf.SerialNumber
}

// newTestLogger returns a logger that writes to the test's output stream.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()

	level := slog.LevelError
	if testing.Verbose() {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: testing.Verbose(),
		Level:     level,
	}))
}
