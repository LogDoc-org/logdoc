package auth

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

const maxBody = 16 << 10

// LoginHandler — POST /api/v1/auth/login {login, password} → a session JWT.
// Unauthenticated by design.
func (s *Service) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Login, Password string }
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		token, id, exp, err := s.Login(req.Login, req.Password)
		if err == errUnauthorized {
			jsonError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"token": token, "login": id.Login, "role": id.Role,
			"expires_at": exp.UTC().Format(time.RFC3339),
		})
	})
}

// MeHandler — GET /api/v1/auth/me: who am I, and is auth even on.
// 401 with mode=user tells the UI to show the login screen.
func (s *Service) MeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Open() {
			writeJSON(w, map[string]any{"mode": "open", "role": RoleAdmin})
			return
		}
		id, ok := s.Identify(credential(r))
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "authentication required", "users": s.users.Load() > 0,
			})
			return
		}
		mode := "user"
		if id.IsKey {
			mode = "key"
		}
		writeJSON(w, map[string]any{"mode": mode, "login": id.Login, "role": id.Role})
	})
}

// UsersHandler — GET (list) / POST (create or update) / DELETE ?login=.
// Wrap with Require(RoleAdmin, ...).
func (s *Service) UsersHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			users, err := s.ListUsers()
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]any{"users": users})
		case http.MethodPost:
			var req struct{ Login, Password, Role string }
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			u, err := s.UpsertUser(req.Login, req.Password, req.Role)
			if err != nil {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, u)
		case http.MethodDelete:
			login := r.URL.Query().Get("login")
			if login == "" {
				jsonError(w, http.StatusBadRequest, "login query parameter is required")
				return
			}
			if err := s.DeleteUser(login); err != nil {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, map[string]string{"deleted": login})
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
}

// TokensHandler — personal tokens of the authenticated user:
// GET (list) / POST {name} (the plaintext comes back once) / DELETE ?id=.
// Wrap with Require(RoleMember, ...).
func (s *Service) TokensHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := From(r.Context())
		if id.Login == "" {
			jsonError(w, http.StatusBadRequest,
				"personal tokens belong to user accounts; log in as a user first")
			return
		}
		switch r.Method {
		case http.MethodGet:
			tokens, err := s.ListTokens(id.Login)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]any{"tokens": tokens})
		case http.MethodPost:
			var req struct{ Name string }
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			plaintext, t, err := s.CreateToken(id.Login, req.Name)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]any{
				"token": plaintext, "id": t.ID, "name": t.Name, "created_at": t.CreatedAt,
			})
		case http.MethodDelete:
			tid, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "id query parameter is required")
				return
			}
			if err := s.DeleteToken(id.Login, tid); err != nil {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, map[string]any{"deleted": tid})
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
