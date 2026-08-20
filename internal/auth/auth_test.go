package auth

import (
	"testing"
	"time"
)

func newSvc(t *testing.T, bootstrapKey string) *Service {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := NewService(store, bootstrapKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestOpenModeAndBootstrapKey(t *testing.T) {
	svc := newSvc(t, "")
	if !svc.Open() {
		t.Fatal("no key, no users: must be open")
	}
	if id, ok := svc.Identify(""); !ok || id.Role != RoleAdmin {
		t.Fatalf("open mode identity: %+v %v", id, ok)
	}

	svc = newSvc(t, "secret-key")
	if svc.Open() {
		t.Fatal("key set: not open")
	}
	if _, ok := svc.Identify("wrong"); ok {
		t.Fatal("wrong key accepted")
	}
	id, ok := svc.Identify("secret-key")
	if !ok || id.Role != RoleAdmin || !id.IsKey {
		t.Fatalf("bootstrap identity: %+v %v", id, ok)
	}
}

func TestLoginAndSessions(t *testing.T) {
	svc := newSvc(t, "")
	if _, err := svc.UpsertUser("alice", "correct horse", "admin"); err != nil {
		t.Fatal(err)
	}
	if svc.Open() {
		t.Fatal("a user exists: auth must be on")
	}

	if _, _, _, err := svc.Login("alice", "wrong password"); err != errUnauthorized {
		t.Fatalf("bad password: %v", err)
	}
	if _, _, _, err := svc.Login("nobody", "correct horse"); err != errUnauthorized {
		t.Fatalf("unknown user: %v", err)
	}

	token, id, exp, err := svc.Login("alice", "correct horse")
	if err != nil || id.Login != "alice" || id.Role != RoleAdmin {
		t.Fatalf("login: %+v %v", id, err)
	}
	if time.Until(exp) < 50*time.Minute {
		t.Fatalf("expiry too soon: %v", exp)
	}
	got, ok := svc.Identify(token)
	if !ok || got.Login != "alice" || got.Role != RoleAdmin || got.IsKey {
		t.Fatalf("jwt identity: %+v %v", got, ok)
	}

	// Tampered signature is rejected.
	if _, ok := svc.Identify(token[:len(token)-2] + "xx"); ok {
		t.Fatal("tampered jwt accepted")
	}

	// A role change applies to the live session immediately.
	if _, err := svc.UpsertUser("bob", "password123", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertUser("alice", "", "member"); err != nil {
		t.Fatal(err)
	}
	if got, _ = svc.Identify(token); got.Role != RoleMember {
		t.Fatalf("demoted role not live: %+v", got)
	}
	// Empty password on update kept the old one.
	if _, _, _, err := svc.Login("alice", "correct horse"); err != nil {
		t.Fatalf("password lost on role update: %v", err)
	}

	// Deleting the user kills the session.
	if err := svc.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Identify(token); ok {
		t.Fatal("session survived user deletion")
	}
}

func TestJWTExpiry(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	token, _ := signJWT(secret, "alice", RoleMember, -time.Minute)
	if _, ok := verifyJWT(secret, token); ok {
		t.Fatal("expired jwt accepted")
	}
	token, _ = signJWT(secret, "alice", RoleMember, time.Minute)
	if login, ok := verifyJWT(secret, token); !ok || login != "alice" {
		t.Fatalf("valid jwt rejected: %q %v", login, ok)
	}
	if _, ok := verifyJWT([]byte("another-secret-another-secret-32"), token); ok {
		t.Fatal("wrong secret accepted")
	}
}

func TestPersonalTokens(t *testing.T) {
	svc := newSvc(t, "")
	if _, err := svc.UpsertUser("alice", "password123", "member"); err != nil {
		t.Fatal(err)
	}
	plaintext, tok, err := svc.CreateToken("alice", "ci")
	if err != nil || plaintext[:4] != tokenPrefix {
		t.Fatalf("create: %q %v", plaintext, err)
	}
	id, ok := svc.Identify(plaintext)
	if !ok || id.Login != "alice" || id.Role != RoleMember {
		t.Fatalf("token identity: %+v %v", id, ok)
	}
	list, err := svc.ListTokens("alice")
	if err != nil || len(list) != 1 || list[0].Name != "ci" {
		t.Fatalf("list: %+v %v", list, err)
	}

	// Revocation is immediate.
	if err := svc.DeleteToken("alice", tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Identify(plaintext); ok {
		t.Fatal("revoked token accepted")
	}
	// A user cannot revoke someone else's token.
	p2, tok2, _ := svc.CreateToken("alice", "x")
	if _, err := svc.UpsertUser("bob", "password123", "member"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteToken("bob", tok2.ID); err == nil {
		t.Fatal("cross-user revoke allowed")
	}
	if _, ok := svc.Identify(p2); !ok {
		t.Fatal("token gone after failed cross-user revoke")
	}
}

func TestUserValidationAndLastAdminGuard(t *testing.T) {
	svc := newSvc(t, "")
	for _, bad := range [][3]string{
		{"", "password123", "admin"},  // no login
		{"x", "short", "admin"},       // short password
		{"x", "password123", "owner"}, // bad role
	} {
		if _, err := svc.UpsertUser(bad[0], bad[1], bad[2]); err == nil {
			t.Fatalf("accepted: %v", bad)
		}
	}

	if _, err := svc.UpsertUser("root", "password123", "admin"); err != nil {
		t.Fatal(err)
	}
	// Without a bootstrap key the last admin is protected.
	if err := svc.DeleteUser("root"); err == nil {
		t.Fatal("deleted the last admin")
	}
	if _, err := svc.UpsertUser("root", "", "member"); err == nil {
		t.Fatal("demoted the last admin")
	}
	// A second admin unblocks it.
	if _, err := svc.UpsertUser("root2", "password123", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteUser("root"); err != nil {
		t.Fatalf("delete with another admin present: %v", err)
	}

	// With a bootstrap key the guard is off (the key is the recovery path).
	svc = newSvc(t, "key")
	if _, err := svc.UpsertUser("solo", "password123", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteUser("solo"); err != nil {
		t.Fatalf("bootstrap key present, delete refused: %v", err)
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := hashPassword("s3cret-pass")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(h, "s3cret-pass") {
		t.Fatal("correct password rejected")
	}
	if verifyPassword(h, "wrong") {
		t.Fatal("wrong password accepted")
	}
	if verifyPassword("garbage", "s3cret-pass") {
		t.Fatal("garbage hash accepted")
	}
}
