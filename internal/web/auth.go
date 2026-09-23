package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"jellybird/internal/auth"
	"jellybird/internal/store"
)

const (
	sessionCookie = "jellybird_session"
	sessionTTL    = 30 * 24 * time.Hour
	// maxFormBytes bounds login/setup bodies; credentials are tiny.
	maxFormBytes = 16 << 10
	// Session tokens are 32 random bytes, base64url: exactly 43 chars.
	sessionTokenLen = 43
)

type ctxKey int

const userCtxKey ctxKey = iota

// userFrom returns the logged-in user attached by the auth middleware.
func userFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}

// sessionUser resolves the session cookie to a user. Only the SHA-256 of the
// cookie is ever looked up, via a parameterized query.
func (h *handlers) sessionUser(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != sessionTokenLen {
		return store.User{}, false
	}
	u, err := h.d.Store.GetSessionUser(r.Context(), auth.HashToken(c.Value))
	if err != nil {
		return store.User{}, false
	}
	return u, true
}

// requireUser gates dashboard pages: no session → login (or first-run setup).
func (h *handlers) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := h.sessionUser(r); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
			return
		}
		if n, err := h.d.Store.CountUsers(r.Context()); err == nil && n == 0 {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	})
}

// apiAuth accepts a dashboard session, or the server token in the
// X-Jellybird-Token header — that's how the Jellybird Jellyfin plugin and
// scripts authenticate, since they have no browser session.
func (h *handlers) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := h.sessionUser(r); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
			return
		}
		if tok := h.d.Config.Server.Token; tok != "" {
			if got := r.Header.Get("X-Jellybird-Token"); got != "" && auth.EqualSecret(got, tok) {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	})
}

// securityHeaders hardens every dashboard/API response against framing,
// MIME sniffing and injected third-party content.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Cache-Control", "no-store")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https://image.tmdb.org data:; "+
			"script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
			"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// --- login / logout / setup ----------------------------------------------

func (h *handlers) loginPage(w http.ResponseWriter, r *http.Request) {
	if n, err := h.d.Store.CountUsers(r.Context()); err == nil && n == 0 {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	if _, ok := h.sessionUser(r); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	h.render(w, r, http.StatusOK, "login.html", map[string]any{"Next": next})
}

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password := r.PostForm.Get("password")
	next := safeNext(r.PostForm.Get("next"))
	fail := func(status int, msg string) {
		h.render(w, r, status, "login.html", map[string]any{"Next": next, "Username": username, "Error": msg})
	}

	ipKey, nameKey := "ip:"+clientIP(r), "user:"+strings.ToLower(username)
	for _, k := range []string{ipKey, nameKey} {
		if ok, wait := h.limiter.Allowed(k); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			fail(http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
			return
		}
	}

	var (
		u   store.User
		err = store.ErrNotFound
	)
	if auth.ValidateUsername(username) == nil {
		u, err = h.d.Store.GetUserByName(r.Context(), username)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.d.Log.Error("login lookup failed", "err", err)
		fail(http.StatusInternalServerError, "Internal error.")
		return
	}
	valid := false
	if err == nil {
		valid = auth.VerifyPassword(u.PasswordHash, password)
	} else {
		auth.BurnVerify(password) // equal timing for unknown users
	}
	if !valid {
		h.limiter.Fail(ipKey)
		h.limiter.Fail(nameKey)
		h.d.Log.Warn("failed dashboard login", "ip", clientIP(r))
		fail(http.StatusUnauthorized, "Invalid username or password.")
		return
	}
	h.limiter.Reset(ipKey)
	h.limiter.Reset(nameKey)
	if err := h.startSession(w, r, u); err != nil {
		h.d.Log.Error("create session failed", "err", err)
		fail(http.StatusInternalServerError, "Internal error.")
		return
	}
	h.d.Log.Info("dashboard login", "user", u.Username, "ip", clientIP(r))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && len(c.Value) == sessionTokenLen {
		_ = h.d.Store.DeleteSession(r.Context(), auth.HashToken(c.Value))
	}
	http.SetCookie(w, h.cookie(r, "", -1))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *handlers) setupPage(w http.ResponseWriter, r *http.Request) {
	if n, err := h.d.Store.CountUsers(r.Context()); err != nil || n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.render(w, r, http.StatusOK, "setup.html", map[string]any{"NeedsToken": h.d.Config.Server.Token != ""})
}

func (h *handlers) setup(w http.ResponseWriter, r *http.Request) {
	if n, err := h.d.Store.CountUsers(r.Context()); err != nil || n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password, confirm := r.PostForm.Get("password"), r.PostForm.Get("confirm")
	needsToken := h.d.Config.Server.Token != ""
	fail := func(status int, msg string) {
		h.render(w, r, status, "setup.html", map[string]any{"NeedsToken": needsToken, "Username": username, "Error": msg})
	}

	ipKey := "ip:" + clientIP(r)
	if ok, wait := h.limiter.Allowed(ipKey); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		fail(http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	// When a server token is configured, claiming the first account requires
	// it, so someone else on the network can't race you to /setup.
	if needsToken && !auth.EqualSecret(r.PostForm.Get("token"), h.d.Config.Server.Token) {
		h.limiter.Fail(ipKey)
		fail(http.StatusForbidden, "Setup token is incorrect.")
		return
	}
	if err := auth.ValidateUsername(username); err != nil {
		fail(http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		fail(http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	if password != confirm {
		fail(http.StatusBadRequest, "Passwords do not match.")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		fail(http.StatusInternalServerError, "Internal error.")
		return
	}
	u, err := h.d.Store.CreateFirstUser(r.Context(), username, hash)
	if errors.Is(err, store.ErrUserExists) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		h.d.Log.Error("create first user failed", "err", err)
		fail(http.StatusInternalServerError, "Internal error.")
		return
	}
	h.d.Log.Info("dashboard admin account created", "user", u.Username, "ip", clientIP(r))
	if err := h.startSession(w, r, u); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// changePassword is POST /api/account/password (JSON). It needs a real
// session — the shared API token has no user to change.
func (h *handlers) changePassword(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "log in to the dashboard to change your password"})
		return
	}
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFormBytes)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	key := "user:" + strings.ToLower(u.Username)
	if ok, _ := h.limiter.Allowed(key); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts, try again later"})
		return
	}
	if !auth.VerifyPassword(u.PasswordHash, body.Current) {
		h.limiter.Fail(key)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "current password is incorrect"})
		return
	}
	if err := auth.ValidatePassword(body.New); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	hash, err := auth.HashPassword(body.New)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// SetPassword revokes every session (other browsers get logged out);
	// issue this browser a fresh one.
	if err := h.d.Store.SetPassword(r.Context(), u.ID, hash); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := h.startSession(w, r, u); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "password changed; please log in again"})
		return
	}
	h.d.Log.Info("dashboard password changed", "user", u.Username)
	writeJSON(w, http.StatusOK, map[string]string{"status": "changed"})
}

// startSession issues a brand-new session token (never reusing one the
// client presented, preventing session fixation) and sets the cookie.
func (h *handlers) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	_ = h.d.Store.PurgeExpiredSessions(r.Context())
	token, digest, err := auth.NewSessionToken()
	if err != nil {
		return err
	}
	if err := h.d.Store.CreateSession(r.Context(), digest, u.ID, time.Now().Add(sessionTTL)); err != nil {
		return err
	}
	http.SetCookie(w, h.cookie(r, token, int(sessionTTL.Seconds())))
	return nil
}

func (h *handlers) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true, // not readable from JS, so an XSS can't steal it
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
	}
}

// safeNext only allows local absolute paths as post-login redirects, so the
// login form can't be abused as an open redirect ("//evil.com", "/\evil").
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	switch {
	case strings.HasPrefix(u.Path, "/login"), strings.HasPrefix(u.Path, "/logout"), strings.HasPrefix(u.Path, "/setup"):
		return "/"
	}
	return u.RequestURI()
}

// clientIP is the TCP peer address. X-Forwarded-For is deliberately ignored:
// it is client-controlled and would let an attacker dodge rate limiting.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
