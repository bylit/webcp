package server

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"webcp/internal/authn"
	"webcp/internal/download"
)

//go:embed web/*
var webAssets embed.FS

type Server struct {
	manager *download.Manager
	logger  *slog.Logger
	assets  http.Handler
}

func New(manager *download.Manager, logger *slog.Logger, authConfig authn.Config) http.Handler {
	content, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err)
	}
	server := &Server{
		manager: manager,
		logger:  logger,
		assets:  http.FileServer(http.FS(content)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.health)
	mux.HandleFunc("/api/config", server.config)
	mux.HandleFunc("/api/filesystem", server.filesystem)
	mux.HandleFunc("/api/downloads", server.downloads)
	mux.HandleFunc("/api/downloads/", server.downloadAction)
	mux.Handle("/", server.assets)
	return securityHeaders(requestLogger(authConfig.Protect(mux), logger))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":         "webcp",
		"default_path": s.manager.DefaultPath(),
	})
}

func (s *Server) filesystem(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listing, err := s.manager.BrowseDirectories(r.URL.Query().Get("path"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, listing)
	case http.MethodPost:
		var input struct {
			Parent string `json:"parent"`
			Name   string `json:"name"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "Request body must contain a parent path and folder name.")
			return
		}
		directory, err := s.manager.CreateDirectory(input.Parent, input.Name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, directory)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"downloads": s.manager.List()})
	case http.MethodPost:
		var input struct {
			URL         string `json:"url"`
			Destination string `json:"destination"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "Request body must contain a URL and destination.")
			return
		}
		job, err := s.manager.Create(input.URL, input.Destination)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) downloadAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/downloads/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "Download not found.")
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := s.manager.Delete(id); err != nil {
			s.managerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "Download action not found.")
		return
	}

	switch parts[1] {
	case "pause":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		job, err := s.manager.Pause(id)
		if err != nil {
			s.managerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
	case "resume":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		job, err := s.manager.Resume(id)
		if err != nil {
			s.managerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
	case "file":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		path, job, err := s.manager.File(id)
		if err != nil {
			s.managerError(w, err)
			return
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": job.Filename}))
		http.ServeFile(w, r, path)
	default:
		writeError(w, http.StatusNotFound, "Download action not found.")
	}
}

func (s *Server) managerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, download.ErrNotFound):
		writeError(w, http.StatusNotFound, "Download not found.")
	case errors.Is(err, download.ErrInvalidState):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.logger.Error("request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "The request could not be completed.")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "Method not allowed.")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func requestLogger(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if logger == nil {
			next.ServeHTTP(w, r)
			return
		}
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/downloads" {
			logger.Info("request", "method", r.Method, "path", filepath.Clean(r.URL.Path), "status", recorder.status)
		}
	})
}
