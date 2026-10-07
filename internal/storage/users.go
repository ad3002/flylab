package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ad3002/flylab/internal/domain"
)

// CreateUser inserts a new account. username must already be normalised (lowercase).
func (s *Store) CreateUser(username, displayName, passwordHash string) (*domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRow(`SELECT 1 FROM users WHERE username = ?`, username).Scan(&exists)
	if err == nil {
		return nil, ErrUsernameTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check username: %w", err)
	}

	now := time.Now().UTC()
	res, err := tx.Exec(
		`INSERT INTO users (username, display_name, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		username, displayName, passwordHash, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("user id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &domain.User{ID: id, Username: username, DisplayName: displayName, CreatedAt: now}, nil
}

// GetUserCredentials returns the user and the stored password hash (for login only).
func (s *Store) GetUserCredentials(username string) (*domain.User, string, error) {
	var u domain.User
	var hash string
	err := s.db.QueryRow(
		`SELECT id, username, display_name, password_hash, created_at FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &u.DisplayName, &hash, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	return &u, hash, nil
}

// UpdatePassword replaces the hash and revokes every session of that user.
// It returns the number of revoked sessions.
func (s *Store) UpdatePassword(username, passwordHash string) (*domain.User, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	var u domain.User
	err = tx.QueryRow(
		`SELECT id, username, display_name, created_at FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &u.DisplayName, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	if _, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, u.ID); err != nil {
		return nil, 0, fmt.Errorf("update password: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, u.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("revoke sessions: %w", err)
	}
	revoked, err := res.RowsAffected()
	if err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return &u, int(revoked), nil
}

// CreateSession stores the SHA-256 of a session token. Expired sessions are purged.
func (s *Store) CreateSession(userID int64, tokenHash string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now); err != nil {
		return fmt.Errorf("purge expired sessions: %w", err)
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, userID, now, expiresAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSessionUser resolves a token hash to its user. Unknown or expired sessions are ErrNotFound.
func (s *Store) GetSessionUser(tokenHash string, now time.Time) (*domain.User, error) {
	var u domain.User
	var expiresAt time.Time
	err := s.db.QueryRow(
		`SELECT u.id, u.username, u.display_name, u.created_at, s.expires_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ?`, tokenHash,
	).Scan(&u.ID, &u.Username, &u.DisplayName, &u.CreatedAt, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !expiresAt.After(now) {
		return nil, ErrNotFound
	}
	return &u, nil
}

// DeleteSession removes a session row (logout). Deleting an unknown session is not an error.
func (s *Store) DeleteSession(tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}
