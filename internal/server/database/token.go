package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rs/xid"
)

var (
	// ErrTokenNotFound is returned when no token exists with the requested
	// identifier or hash.
	ErrTokenNotFound = errors.New("token not found")
	// ErrTokenAlreadyExists is returned when a token collides with one the
	// database already holds, which in practice means a second recovery token.
	ErrTokenAlreadyExists = errors.New("token already exists")
)

type (
	// The Token type represents a credential that authenticates an API caller.
	//
	// The credential itself is not here. The database records only its hash,
	// so a copy of the database holds nothing a caller could present.
	Token struct {
		// The identifier the server assigns to the token, which is what a
		// delete names.
		ID string
		// The hash the presented credential is looked up by.
		Hash string
		// Whether the token is the recovery token or a client token.
		Type string
		// What minted the token: "init", "static", "oidc" or "session".
		Source string
		// The principal the token authenticates as. Empty for the recovery
		// token.
		Principal string
		// The groups the identity provider asserted when the token was
		// minted.
		Groups []string
		// The time after which the token no longer authenticates. Zero when
		// the token does not expire.
		ExpiresAt time.Time
		// The time the token was created.
		CreatedAt time.Time
		// The time the token last authenticated a request. Zero when it never
		// has.
		LastUsedAt time.Time
	}

	// The TokenRepository type provides persistence operations for the token
	// domain.
	TokenRepository struct {
		db *sql.DB
	}
)

// NewTokenRepository returns a TokenRepository backed by the given database.
func NewTokenRepository(db *sql.DB) *TokenRepository {
	return &TokenRepository{db: db}
}

// Create records a token, assigning its identifier and creation time. The
// ExpiresAt, LastUsedAt and ID fields of the input are ignored.
func (r *TokenRepository) Create(ctx context.Context, token Token) (Token, error) {
	const q = `
		INSERT INTO token (id, hash, type, source, principal, asserted_groups, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`

	groups, err := marshalGroups(token.Groups)
	if err != nil {
		return Token{}, err
	}

	token.ID = xid.New().String()
	token.CreatedAt = time.Now().UTC()

	expiresAt := ""
	if !token.ExpiresAt.IsZero() {
		expiresAt = formatTime(token.ExpiresAt)
	}

	_, err = r.db.ExecContext(ctx, q,
		token.ID, token.Hash, token.Type, token.Source, token.Principal,
		groups, expiresAt, formatTime(token.CreatedAt))
	switch {
	case IsUniqueError(err):
		return Token{}, ErrTokenAlreadyExists
	case err != nil:
		return Token{}, fmt.Errorf("failed to insert token: %w", err)
	}

	return token, nil
}

// GetByHash returns the token a presented credential hashes to, reporting
// ErrTokenNotFound when the database holds no such token.
func (r *TokenRepository) GetByHash(ctx context.Context, hash string) (Token, error) {
	const q = `
		SELECT id, hash, type, source, principal, asserted_groups, expires_at, created_at, last_used_at
		FROM token
		WHERE hash = ?
	`

	token, err := scanToken(r.db.QueryRowContext(ctx, q, hash))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Token{}, ErrTokenNotFound
	case err != nil:
		return Token{}, fmt.Errorf("failed to load token: %w", err)
	}

	return token, nil
}

// List returns every token the database records, newest first. This is half
// of the audit the layer promises: with the policy, it answers who can touch
// the server.
func (r *TokenRepository) List(ctx context.Context) ([]Token, error) {
	const q = `
		SELECT id, hash, type, source, principal, asserted_groups, expires_at, created_at, last_used_at
		FROM token
		ORDER BY created_at DESC
	`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query tokens: %w", err)
	}
	defer rows.Close()

	var tokens []Token
	for rows.Next() {
		token, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan token: %w", err)
		}

		tokens = append(tokens, token)
	}

	return tokens, rows.Err()
}

// Delete removes the token with the given identifier, reporting
// ErrTokenNotFound when no such token exists. Revocation is immediate: the
// next request presenting the credential finds nothing to hash to.
func (r *TokenRepository) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM token WHERE id = ?`

	result, err := r.db.ExecContext(ctx, q, id)
	if err != nil {
		return fmt.Errorf("failed to delete token: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to count deleted tokens: %w", err)
	}

	if affected == 0 {
		return ErrTokenNotFound
	}

	return nil
}

// DeleteRecovery removes the recovery token if one exists, which is what
// consuming the reset file does. Deleting when none exists is not an error:
// the reset file asks for a state, not an action.
func (r *TokenRepository) DeleteRecovery(ctx context.Context) error {
	const q = `DELETE FROM token WHERE type = 'recovery'`

	if _, err := r.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("failed to delete the recovery token: %w", err)
	}

	return nil
}

// Touch records that the token authenticated a request at the given time.
func (r *TokenRepository) Touch(ctx context.Context, id string, at time.Time) error {
	const q = `UPDATE token SET last_used_at = ? WHERE id = ?`

	if _, err := r.db.ExecContext(ctx, q, formatTime(at), id); err != nil {
		return fmt.Errorf("failed to record token use: %w", err)
	}

	return nil
}

// DeleteExpired removes every token whose expiry has passed, returning how
// many were removed. An expired token already refuses to authenticate, so
// this is hygiene for the list rather than security.
func (r *TokenRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	const q = `DELETE FROM token WHERE expires_at != '' AND expires_at <= ?`

	result, err := r.db.ExecContext(ctx, q, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("failed to delete expired tokens: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to count deleted tokens: %w", err)
	}

	return affected, nil
}

// scanToken reads one token row from either a row or a rows cursor.
func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var (
		token      Token
		groups     string
		expiresAt  string
		createdAt  string
		lastUsedAt string
	)

	err := row.Scan(&token.ID, &token.Hash, &token.Type, &token.Source, &token.Principal,
		&groups, &expiresAt, &createdAt, &lastUsedAt)
	if err != nil {
		return Token{}, err
	}

	if token.Groups, err = unmarshalGroups(groups); err != nil {
		return Token{}, err
	}

	if token.ExpiresAt, err = parseOptionalTime(expiresAt); err != nil {
		return Token{}, fmt.Errorf("failed to parse expires_at: %w", err)
	}

	if token.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Token{}, fmt.Errorf("failed to parse created_at: %w", err)
	}

	if token.LastUsedAt, err = parseOptionalTime(lastUsedAt); err != nil {
		return Token{}, fmt.Errorf("failed to parse last_used_at: %w", err)
	}

	return token, nil
}

func marshalGroups(groups []string) (string, error) {
	if len(groups) == 0 {
		return "[]", nil
	}

	data, err := json.Marshal(groups)
	if err != nil {
		return "", fmt.Errorf("failed to encode groups: %w", err)
	}

	return string(data), nil
}

func unmarshalGroups(data string) ([]string, error) {
	if data == "" || data == "[]" {
		return nil, nil
	}

	var groups []string
	if err := json.Unmarshal([]byte(data), &groups); err != nil {
		return nil, fmt.Errorf("failed to decode groups: %w", err)
	}

	return groups, nil
}
