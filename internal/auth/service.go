package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const tokenPrefix = "ldt_"

// Identity — who is making the request.
type Identity struct {
	Login string // "" for the bootstrap API key and open dev mode
	Role  Role
	IsKey bool // authenticated with the bootstrap key (not a user account)
}

// Service resolves credentials to identities and manages users and tokens.
type Service struct {
	store        *Store
	secret       []byte
	bootstrapKey string
	sessionTTL   time.Duration
	users        atomic.Int64 // cached COUNT(users): the auth-enabled switch
}

// NewService wires the store with the bootstrap key from the config.
func NewService(store *Store, bootstrapKey string, sessionTTL time.Duration) (*Service, error) {
	secret, err := store.secret()
	if err != nil {
		return nil, fmt.Errorf("auth secret: %w", err)
	}
	n, err := store.userCount()
	if err != nil {
		return nil, err
	}
	if sessionTTL <= 0 {
		sessionTTL = 24 * time.Hour
	}
	s := &Service{store: store, secret: secret, bootstrapKey: bootstrapKey, sessionTTL: sessionTTL}
	s.users.Store(int64(n))
	return s, nil
}

// Open reports dev mode: no bootstrap key and no users = no auth at all.
func (s *Service) Open() bool {
	return s.bootstrapKey == "" && s.users.Load() == 0
}

// Identify resolves a credential: the bootstrap key, a personal token or a
// JWT. Roles always come from the current DB state, so a role change or a
// user deletion applies to live sessions immediately.
func (s *Service) Identify(cred string) (Identity, bool) {
	if s.Open() {
		return Identity{Role: RoleAdmin}, true
	}
	if cred == "" {
		return Identity{}, false
	}
	if s.bootstrapKey != "" &&
		subtle.ConstantTimeCompare([]byte(cred), []byte(s.bootstrapKey)) == 1 {
		return Identity{Role: RoleAdmin, IsKey: true}, true
	}
	var login string
	if strings.HasPrefix(cred, tokenPrefix) {
		h := sha256.Sum256([]byte(cred))
		l, ok, err := s.store.tokenUser(hex.EncodeToString(h[:]))
		if err != nil || !ok {
			return Identity{}, false
		}
		login = l
	} else if l, ok := verifyJWT(s.secret, cred); ok {
		login = l
	} else {
		return Identity{}, false
	}
	role, _, ok, err := s.store.getUser(login)
	if err != nil || !ok {
		return Identity{}, false
	}
	return Identity{Login: login, Role: role}, true
}

// Login checks the password and issues a session JWT.
func (s *Service) Login(login, password string) (token string, id Identity, expires time.Time, err error) {
	role, hash, ok, err := s.store.getUser(login)
	if err != nil {
		return "", Identity{}, time.Time{}, err
	}
	if !ok || !verifyPassword(hash, password) {
		return "", Identity{}, time.Time{}, errUnauthorized
	}
	token, expires = signJWT(s.secret, login, role, s.sessionTTL)
	return token, Identity{Login: login, Role: role}, expires, nil
}

var errUnauthorized = fmt.Errorf("invalid login or password")

// UpsertUser creates a user or updates the role/password of an existing one
// (empty password on update = keep the current one).
func (s *Service) UpsertUser(login, password, role string) (User, error) {
	r, err := parseRole(role)
	if err != nil {
		return User{}, err
	}
	if login == "" {
		return User{}, fmt.Errorf("login is required")
	}
	_, _, exists, err := s.store.getUser(login)
	if err != nil {
		return User{}, err
	}
	if !exists && len(password) < 8 {
		return User{}, fmt.Errorf("password: at least 8 characters")
	}
	if password != "" && len(password) < 8 {
		return User{}, fmt.Errorf("password: at least 8 characters")
	}
	if exists && r == RoleMember {
		if err := s.guardLastAdmin(login); err != nil {
			return User{}, err
		}
	}
	hash := ""
	if password != "" {
		if hash, err = hashPassword(password); err != nil {
			return User{}, err
		}
	}
	if err := s.store.upsertUser(login, hash, r); err != nil {
		return User{}, err
	}
	if !exists {
		s.users.Add(1)
	}
	return User{Login: login, Role: r, CreatedAt: time.Now().UTC()}, nil
}

// DeleteUser removes a user and their tokens.
func (s *Service) DeleteUser(login string) error {
	_, _, exists, err := s.store.getUser(login)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no such user")
	}
	if err := s.guardLastAdmin(login); err != nil {
		return err
	}
	if err := s.store.deleteUser(login); err != nil {
		return err
	}
	s.users.Add(-1)
	return nil
}

// guardLastAdmin refuses to demote or delete the only admin when the
// bootstrap key is not configured (that would lock everyone out).
func (s *Service) guardLastAdmin(login string) error {
	if s.bootstrapKey != "" {
		return nil // the key is always an admin-level recovery path
	}
	role, _, ok, err := s.store.getUser(login)
	if err != nil || !ok || role != RoleAdmin {
		return err
	}
	n, err := s.store.adminCount()
	if err != nil {
		return err
	}
	if n <= 1 {
		return fmt.Errorf("cannot remove the last admin (no bootstrap api_key configured)")
	}
	return nil
}

func (s *Service) ListUsers() ([]User, error) { return s.store.listUsers() }

// CreateToken issues a personal token; the plaintext is returned exactly once.
func (s *Service) CreateToken(login, name string) (plaintext string, t Token, err error) {
	if name == "" {
		name = "token"
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", Token{}, err
	}
	plaintext = tokenPrefix + hex.EncodeToString(raw)
	h := sha256.Sum256([]byte(plaintext))
	id, err := s.store.insertToken(login, name, hex.EncodeToString(h[:]))
	if err != nil {
		return "", Token{}, err
	}
	return plaintext, Token{ID: id, Name: name, CreatedAt: time.Now().UTC()}, nil
}

func (s *Service) ListTokens(login string) ([]Token, error) { return s.store.listTokens(login) }

func (s *Service) DeleteToken(login string, id int64) error {
	ok, err := s.store.deleteToken(login, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such token")
	}
	return nil
}
