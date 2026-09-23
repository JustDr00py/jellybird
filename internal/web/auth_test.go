package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"jellybird/internal/auth"
	"jellybird/internal/config"
	"jellybird/internal/store"
)

const testToken = "s3cret-plugin-token"

func newTestServer(t *testing.T, withUser bool) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "jb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if withUser {
		hash, err := auth.HashPassword("hunter2hunter2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateUser(t.Context(), "admin", hash); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Server.Token = testToken
	r := chi.NewRouter()
	Mount(r, Deps{Config: cfg, Store: st, Log: slog.New(slog.DiscardHandler), Version: "test"})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, st
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestPluginTokenHeaderStillWorks(t *testing.T) {
	srv, _ := newTestServer(t, true)

	req, _ := http.NewRequest("GET", srv.URL+"/api/requests", nil)
	req.Header.Set("X-Jellybird-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plugin token GET: status %d, want 200", resp.StatusCode)
	}

	// POST without browser headers (as the plugin's HttpClient sends) must
	// pass the CSRF guard and token auth.
	req, _ = http.NewRequest("POST", srv.URL+"/api/account/password", strings.NewReader(`{}`))
	req.Header.Set("X-Jellybird-Token", testToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Authenticated, but the token has no user to change → 403, not 401.
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("plugin token POST: status %d, want 403", resp.StatusCode)
	}
}

func TestAPIRejectsOldAuthMethods(t *testing.T) {
	srv, _ := newTestServer(t, true)
	cases := map[string]func(*http.Request){
		"none":         func(*http.Request) {},
		"wrong header": func(r *http.Request) { r.Header.Set("X-Jellybird-Token", "nope") },
		"query token":  func(r *http.Request) { r.URL.RawQuery = "token=" + testToken },
		"basic auth":   func(r *http.Request) { r.SetBasicAuth("x", testToken) },
		"forged cookie": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: strings.Repeat("A", 43)})
		},
		"sqli cookie": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "x' OR '1'='1"})
		},
	}
	for name, mut := range cases {
		req, _ := http.NewRequest("GET", srv.URL+"/api/requests", nil)
		mut(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
}

func TestLoginFlow(t *testing.T) {
	srv, _ := newTestServer(t, true)
	c := newClient()

	resp, _ := c.Get(srv.URL + "/settings")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=") {
		t.Fatalf("unauthenticated page: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"username": {"admin"}, "password": {"wrong-password"}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "Invalid username or password") {
		t.Fatalf("bad password: %d", resp.StatusCode)
	}

	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"username": {"ADMIN"}, "password": {"hunter2hunter2"}, "next": {"//evil.example/x"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var sc *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			sc = ck
		}
	}
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie missing or not hardened: %+v", sc)
	}

	resp, _ = c.Get(srv.URL + "/api/requests")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api with session: %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("security headers missing")
	}

	resp, _ = c.PostForm(srv.URL+"/logout", nil)
	resp.Body.Close()
	resp, _ = c.Get(srv.URL + "/api/requests")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api after logout: %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	srv, _ := newTestServer(t, true)
	c := newClient()
	for i := 0; i < 5; i++ {
		resp, _ := c.PostForm(srv.URL+"/login", url.Values{"username": {"admin"}, "password": {"nope-nope"}})
		resp.Body.Close()
	}
	resp, _ := c.PostForm(srv.URL+"/login", url.Values{"username": {"admin"}, "password": {"hunter2hunter2"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after 5 failures: %d, want 429", resp.StatusCode)
	}
}

func TestCrossOriginPostBlocked(t *testing.T) {
	srv, _ := newTestServer(t, true)
	req, _ := http.NewRequest("POST", srv.URL+"/login", strings.NewReader("username=admin&password=hunter2hunter2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d, want 403", resp.StatusCode)
	}
}

func TestSetup(t *testing.T) {
	srv, st := newTestServer(t, false)
	c := newClient()

	resp, _ := c.Get(srv.URL + "/")
	resp.Body.Close()
	if resp.Header.Get("Location") != "/setup" {
		t.Fatalf("no users should redirect to /setup, got %q", resp.Header.Get("Location"))
	}

	form := url.Values{"username": {"admin"}, "password": {"hunter2hunter2"}, "confirm": {"hunter2hunter2"}, "token": {"wrong"}}
	resp, _ = c.PostForm(srv.URL+"/setup", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("setup with wrong token: %d", resp.StatusCode)
	}

	form.Set("token", testToken)
	form.Set("username", "<script>alert(1)</script>")
	resp, _ = c.PostForm(srv.URL+"/setup", form)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	reflected := strings.Contains(string(body), "<script>alert(1)")
	if resp.StatusCode != http.StatusBadRequest || reflected {
		t.Fatalf("hostile username: status %d, reflected unescaped=%v", resp.StatusCode, reflected)
	}

	form.Set("username", "admin")
	resp, _ = c.PostForm(srv.URL+"/setup", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("setup: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n, _ := st.CountUsers(t.Context()); n != 1 {
		t.Fatalf("users = %d", n)
	}

	// Setup is closed once an account exists.
	form.Set("username", "intruder")
	resp, _ = newClient().PostForm(srv.URL+"/setup", form)
	resp.Body.Close()
	if n, _ := st.CountUsers(t.Context()); n != 1 || resp.Header.Get("Location") != "/login" {
		t.Fatalf("second setup allowed: users=%d", n)
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                       "/",
		"/cloud":                 "/cloud",
		"/?q=a&provider=torbox":  "/?q=a&provider=torbox",
		"//evil.com":             "/",
		"/\\evil.com":            "/",
		"https://evil.com":       "/",
		"javascript:alert(1)":    "/",
		"/login?next=/x":         "/",
		"/ok\r\nLocation: //bad": "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
