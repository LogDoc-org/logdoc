package auth

// Minimal HS256 JWT (RFC 7519) on the standard library — sign and verify
// only what we issue ourselves, no algorithm negotiation.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

type claims struct {
	Sub  string `json:"sub"`  // login
	Role string `json:"role"` // role at issue time (re-checked against the DB)
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func signJWT(secret []byte, login string, role Role, ttl time.Duration) (string, time.Time) {
	now := time.Now()
	exp := now.Add(ttl)
	payload, _ := json.Marshal(claims{Sub: login, Role: string(role), Iat: now.Unix(), Exp: exp.Unix()})
	signing := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), exp
}

// verifyJWT returns the login from a valid, unexpired token.
func verifyJWT(secret []byte, token string) (string, bool) {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return "", false
	}
	signing, sig64 := token[:i], token[i+1:]
	if !strings.HasPrefix(signing, jwtHeader+".") {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sig64)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	if subtle.ConstantTimeCompare(mac.Sum(nil), sig) != 1 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(signing[len(jwtHeader)+1:])
	if err != nil {
		return "", false
	}
	var c claims
	if json.Unmarshal(payload, &c) != nil || c.Sub == "" {
		return "", false
	}
	if time.Now().Unix() >= c.Exp {
		return "", false
	}
	return c.Sub, true
}
