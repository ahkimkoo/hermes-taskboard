// Package server wires together HTTP routing, middleware, and the embedded frontend.
package server

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/attempt"
	"github.com/ahkimkoo/hermes-taskboard/internal/auth"
	"github.com/ahkimkoo/hermes-taskboard/internal/board"
	"github.com/ahkimkoo/hermes-taskboard/internal/config"
	"github.com/ahkimkoo/hermes-taskboard/internal/hermes"
	"github.com/ahkimkoo/hermes-taskboard/internal/sse"
	"github.com/ahkimkoo/hermes-taskboard/internal/store"
	"github.com/ahkimkoo/hermes-taskboard/internal/store/fsstore"
	"github.com/ahkimkoo/hermes-taskboard/internal/uploads"
)

type Server struct {
	Cfg     *config.Store
	Store   *store.Store
	FS      *fsstore.FS
	Pool    *hermes.Pool
	Hub     *sse.Hub
	Board   *board.Service
	Runner  *attempt.Runner
	Auth    *auth.Service
	Logger  *slog.Logger
	Web     fs.FS
	DataDir string

	mu   sync.Mutex
	http atomic.Pointer[http.Server]
}

func New(
	cfg *config.Store, st *store.Store, fs *fsstore.FS, pool *hermes.Pool,
	hub *sse.Hub, b *board.Service, r *attempt.Runner, a *auth.Service,
	logger *slog.Logger, web embed.FS, dataDir string,
) *Server {
	sub, _ := fsGetSub(web, "web")
	return &Server{
		Cfg: cfg, Store: st, FS: fs, Pool: pool, Hub: hub, Board: b, Runner: r, Auth: a,
		Logger: logger, Web: sub, DataDir: dataDir,
	}
}

// uploadsService builds a per-request uploads.Service from current config.
func (s *Server) uploadsService() *uploads.Service {
	c := s.Cfg.Snapshot()
	return &uploads.Service{
		LocalDir:       s.DataDir + "/uploads",
		OSSEnabled:     c.OSS.Enabled && c.OSS.AccessKeyID != "" && (c.OSS.AccessKeySecret != "" || c.OSS.AccessKeySecretEnc != ""),
		OSSEndpoint:    c.OSS.Endpoint,
		OSSBucket:      c.OSS.Bucket,
		OSSAccessKeyID: c.OSS.AccessKeyID,
		OSSSecret:      c.OSS.DecryptedAccessKeySecret(s.Cfg.Secret()),
		OSSPathPrefix:  c.OSS.PathPrefix,
		OSSPublicBase:  c.OSS.PublicBase,
	}
}

func fsGetSub(e embed.FS, dir string) (fs.FS, error) { return fs.Sub(e, dir) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// --- tasks ---
	mux.HandleFunc("/api/tasks", s.withMethod(map[string]http.HandlerFunc{
		"GET":  s.hListTasks,
		"POST": s.hCreateTask,
	}))
	mux.HandleFunc("/api/tasks/", s.routeTasks)

	// --- attempts ---
	mux.HandleFunc("/api/attempts/", s.routeAttempts)

	// --- servers (hermes) ---
	mux.HandleFunc("/api/servers", s.withMethod(map[string]http.HandlerFunc{
		"GET":  s.hListServers,
		"POST": s.hCreateServer,
	}))
	mux.HandleFunc("/api/servers/", s.routeServers)

	// --- tags ---
	mux.HandleFunc("/api/tags", s.withMethod(map[string]http.HandlerFunc{
		"GET":  s.hListTags,
		"POST": s.hUpsertTag,
	}))
	mux.HandleFunc("/api/tags/", s.hDeleteTag)

	// --- settings / config / preferences ---
	mux.HandleFunc("/api/settings", s.withMethod(map[string]http.HandlerFunc{
		"GET": s.hGetSettings,
		"PUT": s.hUpdateSettings,
	}))
	mux.HandleFunc("/api/preferences", s.withMethod(map[string]http.HandlerFunc{
		"GET": s.hGetPreferences,
		"PUT": s.hUpdatePreferences,
	}))
	mux.HandleFunc("/api/config", s.hGetConfig)
	mux.HandleFunc("/api/config/reload", s.hReloadConfig)

	// --- auth ---
	mux.HandleFunc("/api/auth/status", s.hAuthStatus)
	mux.HandleFunc("/api/auth/login", s.hAuthLogin)
	mux.HandleFunc("/api/auth/logout", s.hAuthLogout)
	mux.HandleFunc("/api/auth/enable", s.hAuthEnable)
	mux.HandleFunc("/api/auth/disable", s.hAuthDisable)
	mux.HandleFunc("/api/auth/change", s.hAuthChange)

	// --- streaming ---
	mux.HandleFunc("/api/stream/board", s.hStreamBoard)
	mux.HandleFunc("/api/stream/attempt/", s.hStreamAttempt)

	// --- uploads ---
	mux.HandleFunc("/api/uploads", s.hUploadFile)
	mux.HandleFunc("/uploads/", s.hUploadServe)

	// --- misc ---
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "ts": time.Now().Unix()})
	})

	// --- static / web ---
	mux.HandleFunc("/", s.hStatic)

	// Compose middleware: CORS → auth
	handler := s.Auth.Middleware(isPublic)(mux)
	handler = s.cors(handler)
	handler = s.recovery(handler)
	return handler
}

// Run starts the HTTP server on the listen address from config; blocks.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Cfg.Snapshot().Server.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.http.Store(srv)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shctx)
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// ---- helpers ----

func (s *Server) withMethod(m map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h, ok := m[r.Method]
		if !ok {
			w.Header().Set("Allow", strings.Join(methods(m), ", "))
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func methods(m map[string]http.HandlerFunc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := s.Cfg.Snapshot()
		origin := r.Header.Get("Origin")
		if origin != "" {
			for _, o := range cur.Server.CorsOrigins {
				if o == "*" || strings.EqualFold(o, origin) {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					break
				}
			}
		}
		w.Header().Set("Vary", "Origin")
		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.Logger.Error("panic", "err", rec, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func isPublic(r *http.Request) bool {
	p := r.URL.Path
	if p == "/healthz" || p == "/favicon.svg" || p == "/manifest.webmanifest" {
		return true
	}
	if strings.HasPrefix(p, "/api/auth/login") ||
		strings.HasPrefix(p, "/api/auth/status") ||
		strings.HasPrefix(p, "/api/auth/enable") {
		return true
	}
	// static / web assets + locally-served uploads
	if !strings.HasPrefix(p, "/api/") {
		return true
	}
	return false
}
