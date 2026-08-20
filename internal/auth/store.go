// Package auth — users, roles and access control for the single-node server.
//
// Three credential kinds, all accepted in the same places (X-API-Key,
// Authorization: Bearer, ?api_key=):
//   - the bootstrap API key from the config (full admin, survives everything);
//   - personal tokens ("ldt_..."), issued per user, revocable;
//   - JWT sessions issued by POST /api/v1/auth/login.
//
// With no users and no bootstrap key the server runs in open dev mode.
package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // database/sql driver
)

// Role — what an identity may do. Members read (search, tail, topology);
// admins additionally manage notification rules and users.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

func parseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleAdmin, RoleMember:
		return Role(s), nil
	}
	return "", fmt.Errorf("unknown role %q (admin or member)", s)
}

// User — an account row (the password hash never leaves the package).
type User struct {
	Login     string    `json:"login"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Token — a personal API token (only the hash is stored).
type Token struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Store persists users, tokens and the JWT signing secret in SQLite.
type Store struct {
	db *sql.DB
}

// OpenStore opens (and migrates) the auth database. ":memory:" for tests.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("auth db open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // modernc.org/sqlite: one writer, serialize access
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users (
	login      TEXT PRIMARY KEY,
	pass_hash  TEXT NOT NULL,
	role       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_login TEXT NOT NULL REFERENCES users(login) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	hash       TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("auth db migrate: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// secret returns the persistent JWT signing secret, generating it on first run
// (so sessions survive server restarts).
func (s *Store) secret() ([]byte, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'jwt_secret'`).Scan(&v)
	if err == nil {
		return hex.DecodeString(v)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	v = hex.EncodeToString(raw)
	if _, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES ('jwt_secret', ?)`, v); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *Store) userCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) adminCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, string(RoleAdmin)).Scan(&n)
	return n, err
}

func (s *Store) getUser(login string) (role Role, passHash string, ok bool, err error) {
	var r string
	err = s.db.QueryRow(`SELECT role, pass_hash FROM users WHERE login = ?`, login).
		Scan(&r, &passHash)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return Role(r), passHash, true, nil
}

func (s *Store) upsertUser(login, passHash string, role Role) error {
	_, err := s.db.Exec(`
INSERT INTO users (login, pass_hash, role, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT(login) DO UPDATE SET
	pass_hash = CASE WHEN excluded.pass_hash != '' THEN excluded.pass_hash ELSE pass_hash END,
	role      = excluded.role`,
		login, passHash, string(role), time.Now().Unix())
	return err
}

func (s *Store) deleteUser(login string) error {
	// Tokens go with the user (no PRAGMA foreign_keys needed — explicit).
	if _, err := s.db.Exec(`DELETE FROM tokens WHERE user_login = ?`, login); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM users WHERE login = ?`, login)
	return err
}

func (s *Store) listUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT login, role, created_at FROM users ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var r string
		var ts int64
		if err := rows.Scan(&u.Login, &r, &ts); err != nil {
			return nil, err
		}
		u.Role = Role(r)
		u.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) insertToken(login, name, hash string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO tokens (user_login, name, hash, created_at) VALUES (?, ?, ?, ?)`,
		login, name, hash, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) tokenUser(hash string) (login string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT user_login FROM tokens WHERE hash = ?`, hash).Scan(&login)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return login, true, nil
}

func (s *Store) listTokens(login string) ([]Token, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM tokens WHERE user_login = ? ORDER BY id`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		var ts int64
		if err := rows.Scan(&t.ID, &t.Name, &ts); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) deleteToken(login string, id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM tokens WHERE user_login = ? AND id = ?`, login, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
