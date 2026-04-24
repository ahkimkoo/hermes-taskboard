package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/ahkimkoo/hermes-taskboard/internal/auth"
	"github.com/ahkimkoo/hermes-taskboard/internal/userdir"
)

// routeUsers dispatches /api/users — GET lists every user, POST creates one.
// Admin-only (enforced at the mux level via auth.RequireAdmin).
func (s *Server) routeUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		s.hListUsers(w, r)
	case "POST":
		s.hCreateUser(w, r)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// routeUserItem dispatches /api/users/{username}[/password|/disabled].
func (s *Server) routeUserItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/users/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	username := parts[0]
	switch {
	case len(parts) == 1 && r.Method == "DELETE":
		s.hDeleteUser(w, r, username)
	case len(parts) == 2 && parts[1] == "password" && r.Method == "POST":
		s.hAdminSetPassword(w, r, username)
	case len(parts) == 2 && parts[1] == "disabled" && r.Method == "PATCH":
		s.hSetUserDisabled(w, r, username)
	default:
		http.NotFound(w, r)
	}
}

type userDTO struct {
	Username  string `json:"username"`
	IsAdmin   bool   `json:"is_admin"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"created_at,omitempty"`
}

func (s *Server) hListUsers(w http.ResponseWriter, r *http.Request) {
	users := s.Users.List()
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		out = append(out, userDTO{
			Username:  u.Username,
			IsAdmin:   u.IsAdmin,
			Disabled:  s.Users.IsDisabled(u.Username),
			CreatedAt: u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writeJSON(w, 200, map[string]any{"users": out})
}

type createUserReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"is_admin"`
}

func (s *Server) hCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Password) < 8 {
		writeErr(w, 400, errors.New("username + password (≥8 chars) required"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	u := &userdir.UserConfig{
		Username:     req.Username,
		PasswordHash: string(hash),
		IsAdmin:      req.IsAdmin,
	}
	if err := s.Users.Create(u); err != nil {
		writeErr(w, 400, err)
		return
	}
	// Ensure their DB dir exists right away so downstream subsystems can
	// find it without racing the first login.
	_, _ = s.Stores.Get(req.Username)
	writeJSON(w, 201, map[string]any{"user": userDTO{
		Username: u.Username, IsAdmin: u.IsAdmin,
	}})
}

func (s *Server) hDeleteUser(w http.ResponseWriter, r *http.Request, username string) {
	me := auth.UserFromContext(r.Context())
	if me != nil && me.Username == username {
		writeErr(w, 400, errors.New("cannot delete yourself"))
		return
	}
	// Refuse to delete the last admin so the board never becomes headless.
	target, ok := s.Users.Get(username)
	if !ok {
		writeErr(w, 404, errors.New("not found"))
		return
	}
	if target.IsAdmin {
		count := 0
		for _, u := range s.Users.List() {
			if u.IsAdmin {
				count++
			}
		}
		if count <= 1 {
			writeErr(w, 400, errors.New("cannot delete the last admin"))
			return
		}
	}
	// Evict cached DB handles first, then rm -rf the dir.
	s.Stores.Evict(username)
	s.FS.Evict(username)
	if err := s.Users.Delete(username); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.ReloadPool()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hAdminSetPassword(w http.ResponseWriter, r *http.Request, username string) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Auth.AdminSetPassword(r.Context(), username, req.Password); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hSetUserDisabled(w http.ResponseWriter, r *http.Request, username string) {
	me := auth.UserFromContext(r.Context())
	if me != nil && me.Username == username {
		writeErr(w, 400, errors.New("cannot disable yourself"))
		return
	}
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Users.SetDisabled(username, req.Disabled); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ensure errors + fmt imports aren't pruned
var _ = fmt.Sprintf
