package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestMux wires the handlers the way main.go does.
func newTestMux(t *testing.T, bootstrapKey string) (*http.ServeMux, *Service) {
	t.Helper()
	svc := newSvc(t, bootstrapKey)
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/auth/login", svc.LoginHandler())
	mux.Handle("GET /api/v1/auth/me", svc.MeHandler())
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.Handle(m+" /api/v1/auth/tokens", svc.Require(RoleMember, svc.TokensHandler()))
		mux.Handle(m+" /api/v1/users", svc.Require(RoleAdmin, svc.UsersHandler()))
	}
	mux.Handle("GET /api/v1/query", svc.Require(RoleMember,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })))
	mux.Handle("POST /api/v1/notify/rules", svc.Require(RoleAdmin,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })))
	return mux, svc
}

func do(t *testing.T, mux *http.ServeMux, method, path, cred, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cred != "" {
		req.Header.Set("X-API-Key", cred)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestHTTPRoleEnforcement(t *testing.T) {
	mux, svc := newTestMux(t, "boot-key")

	// Bootstrap key creates two users with different roles.
	code, _ := do(t, mux, "POST", "/api/v1/users", "boot-key",
		`{"login":"alice","password":"password123","role":"admin"}`)
	if code != 200 {
		t.Fatalf("create admin: %d", code)
	}
	if code, _ = do(t, mux, "POST", "/api/v1/users", "boot-key",
		`{"login":"bob","password":"password123","role":"member"}`); code != 200 {
		t.Fatalf("create member: %d", code)
	}

	// Both log in.
	login := func(user string) string {
		code, out := do(t, mux, "POST", "/api/v1/auth/login", "",
			`{"login":"`+user+`","password":"password123"}`)
		if code != 200 {
			t.Fatalf("login %s: %d %v", user, code, out)
		}
		return out["token"].(string)
	}
	aliceJWT, bobJWT := login("alice"), login("bob")

	// Everyone reads; only the admin writes rules or manages users.
	cases := []struct {
		cred, method, path string
		want               int
	}{
		{"", "GET", "/api/v1/query", 401},
		{bobJWT, "GET", "/api/v1/query", 200},
		{aliceJWT, "GET", "/api/v1/query", 200},
		{bobJWT, "POST", "/api/v1/notify/rules", 403},
		{aliceJWT, "POST", "/api/v1/notify/rules", 200},
		{bobJWT, "GET", "/api/v1/users", 403},
		{aliceJWT, "GET", "/api/v1/users", 200},
		{"garbage", "GET", "/api/v1/query", 401},
	}
	for _, c := range cases {
		if code, _ := do(t, mux, c.method, c.path, c.cred, `{}`); code != c.want {
			t.Fatalf("%s %s as %.12q = %d, want %d", c.method, c.path, c.cred, code, c.want)
		}
	}

	// /me reflects the identity.
	if _, out := do(t, mux, "GET", "/api/v1/auth/me", bobJWT, ""); out["role"] != "member" || out["mode"] != "user" {
		t.Fatalf("me: %v", out)
	}
	if _, out := do(t, mux, "GET", "/api/v1/auth/me", "boot-key", ""); out["mode"] != "key" {
		t.Fatalf("me key: %v", out)
	}

	// The member issues a personal token, uses it, revokes it.
	code, out := do(t, mux, "POST", "/api/v1/auth/tokens", bobJWT, `{"name":"ci"}`)
	if code != 200 {
		t.Fatalf("token create: %d %v", code, out)
	}
	personal := out["token"].(string)
	if code, _ = do(t, mux, "GET", "/api/v1/query", personal, ""); code != 200 {
		t.Fatalf("personal token read: %d", code)
	}
	if code, _ = do(t, mux, "POST", "/api/v1/notify/rules", personal, `{}`); code != 403 {
		t.Fatalf("member token must not write rules: %d", code)
	}
	id := int64(out["id"].(float64))
	if code, _ = do(t, mux, "DELETE",
		"/api/v1/auth/tokens?id="+jsonNum(id), bobJWT, ""); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ = do(t, mux, "GET", "/api/v1/query", personal, ""); code != 401 {
		t.Fatalf("revoked token still works: %d", code)
	}

	// The bootstrap-key identity has no user account for tokens.
	if code, _ = do(t, mux, "POST", "/api/v1/auth/tokens", "boot-key", `{}`); code != 400 {
		t.Fatalf("key identity token create: %d", code)
	}

	_ = svc
}

func TestHTTPOpenMode(t *testing.T) {
	mux, _ := newTestMux(t, "")
	if code, out := do(t, mux, "GET", "/api/v1/auth/me", "", ""); code != 200 || out["mode"] != "open" {
		t.Fatalf("open me: %d %v", code, out)
	}
	if code, _ := do(t, mux, "GET", "/api/v1/query", "", ""); code != 200 {
		t.Fatal("open mode must not require auth")
	}
	// Creating the first user turns auth on.
	if code, _ := do(t, mux, "POST", "/api/v1/users", "",
		`{"login":"root","password":"password123","role":"admin"}`); code != 200 {
		t.Fatal("first user")
	}
	if code, _ := do(t, mux, "GET", "/api/v1/query", "", ""); code != 401 {
		t.Fatal("auth must be on after the first user")
	}
}

func TestSessionExpiry(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err := NewService(store, "", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertUser("alice", "password123", "admin"); err != nil {
		t.Fatal(err)
	}
	token, _, _, err := svc.Login("alice", "password123")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // exp has 1s resolution
	if _, ok := svc.Identify(token); ok {
		t.Fatal("expired session accepted")
	}
}

func jsonNum(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
