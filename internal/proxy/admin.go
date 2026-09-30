package proxy

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var adminAssets embed.FS

type adminSession struct {
	CSRF    string
	Expires time.Time
}
type adminSessions struct {
	mu    sync.Mutex
	items map[string]adminSession
}

const adminCookie = "workersai_admin"

// Reverse proxies should overwrite forwarding headers instead of accepting client values.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	if proto == "http" || proto == "https" {
		scheme = proto
	}
	forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if forwarded != "" {
		parsed, err := url.Parse(scheme + "://" + forwarded)
		if err == nil && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" {
			host = parsed.Host
		}
	}
	return scheme + "://" + host
}
func (s *Server) session(r *http.Request) (adminSession, bool) {
	cookie, err := r.Cookie(adminCookie)
	if err != nil {
		return adminSession{}, false
	}
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	session, ok := s.sessions.items[cookie.Value]
	if !ok || time.Now().After(session.Expires) {
		delete(s.sessions.items, cookie.Value)
		return adminSession{}, false
	}
	return session, true
}
func (s *Server) admin(w *responseWriter, r *http.Request) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.URL.Path == "/" || r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/admin/api/") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, "chat", 405, "method not allowed")
			return
		}
		name := ""
		mime := ""
		switch r.URL.Path {
		case "/admin/":
			name = "index.html"
			mime = "text/html; charset=utf-8"
		case "/admin/theme.js":
			name = "theme.js"
			mime = "text/javascript; charset=utf-8"
		case "/admin/app.js":
			name = "app.js"
			mime = "text/javascript; charset=utf-8"
		case "/admin/app.css":
			name = "app.css"
			mime = "text/css; charset=utf-8"
		default:
			writeError(w, "chat", 404, "endpoint not found")
			return
		}
		data, err := adminAssets.ReadFile("web/" + name)
		if err != nil {
			writeError(w, "chat", 500, "management assets unavailable")
			return
		}
		w.Header().Set("Content-Type", mime)
		w.WriteHeader(200)
		if r.Method != http.MethodHead {
			w.Write(data)
		}
		return
	}
	if r.URL.Path == "/admin/api/session" && r.Method == http.MethodPost {
		s.login(w, r)
		return
	}
	session, ok := s.session(r)
	if !ok {
		writeError(w, "chat", 401, "请使用 AUTH_TOKEN 登录管理页面")
		return
	}
	if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRF)) != 1 {
		writeError(w, "chat", 403, "管理会话校验失败，请刷新页面重试")
		return
	}
	switch {
	case r.URL.Path == "/admin/api/stats" || r.URL.Path == "/admin/api/logs" || strings.HasPrefix(r.URL.Path, "/admin/api/logs/"):
		s.adminLogs(w, r)
	case r.URL.Path == "/admin/api/session" && r.Method == http.MethodDelete:
		cookie, _ := r.Cookie(adminCookie)
		s.sessions.mu.Lock()
		delete(s.sessions.items, cookie.Value)
		s.sessions.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: adminCookie, Path: "/admin/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: strings.HasPrefix(requestBaseURL(r), "https:"), MaxAge: -1})
		w.WriteHeader(204)
	case r.URL.Path == "/admin/api/state" && r.Method == http.MethodGet:
		s.catalog.mu.RLock()
		catalog := map[string]any{"count": len(s.catalog.routes), "last_attempt": s.catalog.lastAttempt, "last_success": s.catalog.lastSuccess, "last_error": s.catalog.lastError, "refresh_interval_seconds": 3600}
		s.catalog.mu.RUnlock()
		writeJSON(w, 200, map[string]any{"csrf_token": session.CSRF, "accounts": s.accounts.views(), "catalog": catalog, "started_at": s.startedAt, "routing": "round_robin"})
	case r.URL.Path == "/admin/api/models" && r.Method == http.MethodGet:
		// Copy routes and tasks under one lock to avoid mismatched refresh snapshots.
		s.catalog.mu.RLock()
		routes := make(map[string]string, len(s.catalog.routes))
		tasks := make(map[string][]string, len(s.catalog.tasks))
		for name, id := range s.catalog.routes {
			routes[name] = id
			tasks[id] = append([]string{}, s.catalog.tasks[id]...)
		}
		s.catalog.mu.RUnlock()
		names := make([]string, 0, len(routes))
		for name := range routes {
			names = append(names, name)
		}
		sort.Strings(names)
		data := make([]any, 0, len(names))
		for _, name := range names {
			id := routes[name]
			data = append(data, map[string]any{"id": name, "upstream_id": id, "tasks": tasks[id], "supported_endpoints": s.modelEndpoints(id)})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	case r.URL.Path == "/admin/api/models/refresh" && r.Method == http.MethodPost:
		if err := s.RefreshModels(r.Context()); err != nil {
			writeError(w, "chat", 502, "模型目录刷新失败，已保留现有目录")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/admin/api/accounts" && r.Method == http.MethodPost:
		s.mutateAccount(w, r, "", false)
	case strings.HasPrefix(r.URL.Path, "/admin/api/accounts/") && (r.Method == http.MethodPatch || r.Method == http.MethodDelete):
		id := strings.TrimPrefix(r.URL.Path, "/admin/api/accounts/")
		if id == "" || strings.Contains(id, "/") {
			writeError(w, "chat", 404, "账号不存在")
			return
		}
		s.mutateAccount(w, r, id, r.Method == http.MethodDelete)
	default:
		writeError(w, "chat", 404, "endpoint not found")
	}
}
func (s *Server) login(w *responseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AuthToken)) != 1 {
		writeError(w, "chat", 401, "管理密钥不正确")
		return
	}
	id, csrf := randomID(), randomID()
	now := time.Now()
	s.sessions.mu.Lock()
	if s.sessions.items == nil {
		s.sessions.items = map[string]adminSession{}
	}
	for key, session := range s.sessions.items {
		if now.After(session.Expires) {
			delete(s.sessions.items, key)
		}
	}
	if cookie, err := r.Cookie(adminCookie); err == nil {
		delete(s.sessions.items, cookie.Value)
	}
	if len(s.sessions.items) >= 256 {
		s.sessions.mu.Unlock()
		writeError(w, "chat", 429, "管理会话过多，请稍后重试")
		return
	}
	s.sessions.items[id] = adminSession{CSRF: csrf, Expires: now.Add(24 * time.Hour)}
	s.sessions.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: id, Path: "/admin/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: strings.HasPrefix(requestBaseURL(r), "https:"), MaxAge: 86400})
	writeJSON(w, 200, map[string]string{"csrf_token": csrf})
}
func (s *Server) mutateAccount(w *responseWriter, r *http.Request, id string, remove bool) {
	var patch accountUpdate
	if !remove {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&patch); err != nil {
			writeError(w, "chat", 400, "账号数据格式错误或包含未知字段")
			return
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			writeError(w, "chat", 400, "请求体只能包含一个 JSON 对象")
			return
		}
	}
	created := id == ""
	id, err := s.accounts.update(id, patch, remove)
	if err != nil {
		status := 500
		var failure *apiFailure
		if errors.As(err, &failure) {
			status = failure.status
		}
		writeError(w, "chat", status, err.Error())
		return
	}
	if remove {
		w.WriteHeader(204)
		return
	}
	status := 200
	if created {
		status = 201
	}
	for _, view := range s.accounts.views() {
		if view.ID == id {
			writeJSON(w, status, view)
			return
		}
	}
	// Another administrator may delete it immediately after this successful write.
	writeJSON(w, status, map[string]string{"id": id})
}
