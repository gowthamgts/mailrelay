package httpauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/gowthamgts/mailrelay/internal/models"
)

const (
	testClientID     = "test-client"
	testClientSecret = "test-secret"
	testCode         = "good-code"
)

// fakeProvider is a minimal OIDC provider: discovery, JWKS and a token
// endpoint that returns an ID token built by the current test.
type fakeProvider struct {
	t       *testing.T
	srv     *httptest.Server
	key     *rsa.PrivateKey
	keyID   string
	mu      sync.Mutex
	claims  map[string]any
	signer  *rsa.PrivateKey // key used to sign; defaults to key
	noIDTok bool
	// codes maps extra authorization codes to the claims they redeem for.
	codes map[string]map[string]any
	// lastRedirectURI is the redirect_uri sent to the token endpoint.
	lastRedirectURI string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{t: t, key: key, keyID: "k1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		iss := fp.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss,
			"authorization_endpoint":                iss + "/authorize",
			"token_endpoint":                        iss + "/token",
			"jwks_uri":                              iss + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &fp.key.PublicKey, KeyID: fp.keyID, Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, secret, ok := r.BasicAuth()
		if !ok {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		fp.mu.Lock()
		claims, known := fp.codes[r.PostForm.Get("code")]
		if !known && r.PostForm.Get("code") == testCode {
			claims, known = fp.claims, true
		}
		fp.mu.Unlock()
		if id != testClientID || secret != testClientSecret || !known {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		fp.mu.Lock()
		fp.lastRedirectURI = r.PostForm.Get("redirect_uri")
		resp := map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600}
		if !fp.noIDTok {
			resp["id_token"] = fp.sign(claims)
		}
		fp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	fp.srv = httptest.NewServer(mux)
	t.Cleanup(fp.srv.Close)
	return fp
}

func (fp *fakeProvider) sign(claims map[string]any) string {
	fp.t.Helper()
	key := fp.signer
	if key == nil {
		key = fp.key
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", fp.keyID))
	if err != nil {
		fp.t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := sig.Sign(payload)
	if err != nil {
		fp.t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		fp.t.Fatal(err)
	}
	return s
}

// defaultClaims returns a valid claim set for the given nonce.
func (fp *fakeProvider) defaultClaims(nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":            fp.srv.URL,
		"aud":            testClientID,
		"sub":            "user-123",
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
		"nonce":          nonce,
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
}

func (fp *fakeProvider) setClaims(c map[string]any) {
	fp.mu.Lock()
	fp.claims = c
	fp.mu.Unlock()
}

func (fp *fakeProvider) authConfig() models.HTTPAuthConfig {
	return models.HTTPAuthConfig{
		Enabled:    true,
		SessionTTL: time.Hour,
		Providers: []models.HTTPAuthProviderConfig{{
			Name:         "fake",
			IssuerURL:    fp.srv.URL,
			ClientID:     testClientID,
			ClientSecret: testClientSecret,
		}},
	}
}

var localHTTP = models.HTTPConfig{Addr: "127.0.0.1:2623"}

func newTestManager(t *testing.T, cfg models.HTTPAuthConfig) *Manager {
	t.Helper()
	m, err := NewManager(context.Background(), cfg, localHTTP)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// testApp mounts the auth endpoints and a protected handler like main.go does.
func testApp(m *Manager) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/auth/login", m.LoginHandler())
	mux.Handle("/auth/callback", m.CallbackHandler())
	mux.Handle("/auth/logout", m.LogoutHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		u := CurrentUser(r)
		if u == nil {
			http.Error(w, "no user", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(u.Subject + "|" + u.Email + "|" + u.Name + "|" + u.Provider))
	})
	return m.AuthMiddleware(mux, func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/auth/") || r.URL.Path == "/healthz"
	})
}

func do(t *testing.T, h http.Handler, method, target string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = "localhost:2623"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func findCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

type loginStart struct {
	state, nonce string
	cookie       *http.Cookie
	authURL      *url.URL
}

// beginLogin requests target unauthenticated and returns the redirect details.
func beginLogin(t *testing.T, h http.Handler, method, target string) loginStart {
	t.Helper()
	rec := do(t, h, method, target, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 to provider, got %d: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	c := findCookie(t, rec, stateCookieName)
	if c == nil {
		t.Fatal("state cookie not set")
	}
	return loginStart{state: loc.Query().Get("state"), nonce: loc.Query().Get("nonce"), cookie: c, authURL: loc}
}

func callback(t *testing.T, h http.Handler, ls loginStart, state, code string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{"state": {state}, "code": {code}}
	var cookies []*http.Cookie
	if ls.cookie != nil {
		cookies = append(cookies, ls.cookie)
	}
	return do(t, h, http.MethodGet, "/auth/callback?"+q.Encode(), cookies)
}

func TestLoginFlowSuccess(t *testing.T) {
	fp := newFakeProvider(t)
	m := newTestManager(t, fp.authConfig())
	h := testApp(m)

	ls := beginLogin(t, h, http.MethodGet, "/emails?page=2")
	if got := ls.authURL.Scheme + "://" + ls.authURL.Host + ls.authURL.Path; got != fp.srv.URL+"/authorize" {
		t.Fatalf("auth endpoint = %s", got)
	}
	q := ls.authURL.Query()
	if q.Get("client_id") != testClientID || q.Get("response_type") != "code" {
		t.Fatalf("unexpected auth params: %v", q)
	}
	if q.Get("redirect_uri") != "http://localhost:2623/auth/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if !strings.Contains(q.Get("scope"), "openid") || ls.state == "" || ls.nonce == "" {
		t.Fatalf("missing scope/state/nonce: %v", q)
	}
	if !ls.cookie.HttpOnly || ls.cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie flags: %+v", ls.cookie)
	}

	fp.setClaims(fp.defaultClaims(ls.nonce))
	rec := callback(t, h, ls, ls.state, testCode)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/emails?page=2" {
		t.Fatalf("post-login redirect = %q", loc)
	}
	if fp.lastRedirectURI != "http://localhost:2623/auth/callback" {
		t.Fatalf("token redirect_uri = %q", fp.lastRedirectURI)
	}
	if c := findCookie(t, rec, stateCookieName); c == nil || c.MaxAge >= 0 {
		t.Fatalf("state cookie should be cleared: %+v", c)
	}
	sess := findCookie(t, rec, "mailrelay_session")
	if sess == nil {
		t.Fatal("session cookie not set")
	}

	rec = do(t, h, http.MethodGet, "/", []*http.Cookie{sess})
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated request = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "user-123|alice@example.com|Alice|fake" {
		t.Fatalf("user = %q", got)
	}

	// Replaying the callback with the (now consumed) state still in hand
	// must not matter: a new state cookie is needed for every login.
	rec = callback(t, h, loginStart{}, ls.state, testCode)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("callback without state cookie = %d", rec.Code)
	}
}

func TestLoginRejections(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		cfg     func(*models.HTTPAuthConfig)
		claims  func(c map[string]any)
		setup   func(fp *fakeProvider)
		state   func(ls loginStart) string
		code    string
		want    int
		noState bool
	}{
		{name: "bad signature", setup: func(fp *fakeProvider) { fp.signer = other }, want: http.StatusUnauthorized},
		{name: "wrong audience", claims: func(c map[string]any) { c["aud"] = "someone-else" }, want: http.StatusUnauthorized},
		{name: "wrong issuer", claims: func(c map[string]any) { c["iss"] = "https://evil.example" }, want: http.StatusUnauthorized},
		{name: "expired token", claims: func(c map[string]any) {
			c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
			c["exp"] = time.Now().Add(-time.Hour).Unix()
		}, want: http.StatusUnauthorized},
		{name: "nonce mismatch", claims: func(c map[string]any) { c["nonce"] = "other" }, want: http.StatusUnauthorized},
		{name: "no id_token", setup: func(fp *fakeProvider) { fp.noIDTok = true }, want: http.StatusUnauthorized},
		{name: "bad code", code: "bad-code", want: http.StatusUnauthorized},
		{name: "state mismatch", state: func(loginStart) string { return "forged" }, want: http.StatusBadRequest},
		{name: "missing state cookie", noState: true, want: http.StatusBadRequest},
		{name: "missing code", code: "-", want: http.StatusBadRequest},
		{
			name: "disallowed domain",
			cfg:  func(c *models.HTTPAuthConfig) { c.AllowedEmailDomains = []string{"corp.example"} },
			want: http.StatusForbidden,
		},
		{
			name:   "unverified email with domain restriction",
			cfg:    func(c *models.HTTPAuthConfig) { c.AllowedEmailDomains = []string{"example.com"} },
			claims: func(c map[string]any) { c["email_verified"] = false },
			want:   http.StatusForbidden,
		},
		{
			name:   "missing email_verified with domain restriction",
			cfg:    func(c *models.HTTPAuthConfig) { c.AllowedEmailDomains = []string{"example.com"} },
			claims: func(c map[string]any) { delete(c, "email_verified") },
			want:   http.StatusForbidden,
		},
		{
			name: "allowed domain",
			cfg:  func(c *models.HTTPAuthConfig) { c.AllowedEmailDomains = []string{" Example.COM "} },
			want: http.StatusSeeOther,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := newFakeProvider(t)
			cfg := fp.authConfig()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			if tt.setup != nil {
				tt.setup(fp)
			}
			h := testApp(newTestManager(t, cfg))
			ls := beginLogin(t, h, http.MethodGet, "/")
			claims := fp.defaultClaims(ls.nonce)
			if tt.claims != nil {
				tt.claims(claims)
			}
			fp.setClaims(claims)

			state := ls.state
			if tt.state != nil {
				state = tt.state(ls)
			}
			code := testCode
			switch tt.code {
			case "":
			case "-":
				code = ""
			default:
				code = tt.code
			}
			if tt.noState {
				ls.cookie = nil
			}
			rec := callback(t, h, ls, state, code)
			if rec.Code != tt.want {
				t.Fatalf("callback = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want != http.StatusSeeOther && findCookie(t, rec, "mailrelay_session") != nil {
				t.Fatal("session cookie must not be set on failure")
			}
		})
	}
}

func TestCallbackProviderError(t *testing.T) {
	fp := newFakeProvider(t)
	h := testApp(newTestManager(t, fp.authConfig()))
	rec := do(t, h, http.MethodGet, "/auth/callback?error=access_denied&state=x", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestTamperedStateCookie(t *testing.T) {
	fp := newFakeProvider(t)
	h := testApp(newTestManager(t, fp.authConfig()))
	ls := beginLogin(t, h, http.MethodGet, "/")
	ls.cookie.Value = "x" + ls.cookie.Value
	if rec := callback(t, h, ls, ls.state, testCode); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestForgedSessionCookieRejected(t *testing.T) {
	fp := newFakeProvider(t)
	h := testApp(newTestManager(t, fp.authConfig()))
	rec := do(t, h, http.MethodGet, "/", []*http.Cookie{{Name: "mailrelay_session", Value: "garbage"}})
	if rec.Code != http.StatusFound {
		t.Fatalf("forged session should redirect to login, got %d", rec.Code)
	}
}

func TestSessionFromOtherInstanceRejected(t *testing.T) {
	fp := newFakeProvider(t)
	cfg := fp.authConfig()
	cfg.SessionSecret = "one"
	h1 := testApp(newTestManager(t, cfg))
	cfg.SessionSecret = "two"
	h2 := testApp(newTestManager(t, cfg))

	ls := beginLogin(t, h1, http.MethodGet, "/")
	fp.setClaims(fp.defaultClaims(ls.nonce))
	sess := findCookie(t, callback(t, h1, ls, ls.state, testCode), "mailrelay_session")
	if sess == nil {
		t.Fatal("no session")
	}
	if rec := do(t, h2, http.MethodGet, "/", []*http.Cookie{sess}); rec.Code != http.StatusFound {
		t.Fatalf("session signed with another secret accepted: %d", rec.Code)
	}

	// The same secret shares sessions across instances.
	cfg.SessionSecret = "one"
	h3 := testApp(newTestManager(t, cfg))
	if rec := do(t, h3, http.MethodGet, "/", []*http.Cookie{sess}); rec.Code != http.StatusOK {
		t.Fatalf("shared secret session rejected: %d", rec.Code)
	}
}

func TestLogout(t *testing.T) {
	fp := newFakeProvider(t)
	h := testApp(newTestManager(t, fp.authConfig()))
	ls := beginLogin(t, h, http.MethodGet, "/")
	fp.setClaims(fp.defaultClaims(ls.nonce))
	sess := findCookie(t, callback(t, h, ls, ls.state, testCode), "mailrelay_session")

	rec := do(t, h, http.MethodGet, "/auth/logout", []*http.Cookie{sess})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("logout = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cleared := findCookie(t, rec, "mailrelay_session")
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("session cookie not cleared: %+v", cleared)
	}

	// A browser drops the cookie; without it the user must log in again.
	if rec := do(t, h, http.MethodGet, "/", nil); rec.Code != http.StatusFound {
		t.Fatalf("after logout = %d", rec.Code)
	}

	// Logout without a session is harmless.
	if rec := do(t, h, http.MethodGet, "/auth/logout", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout without session = %d", rec.Code)
	}
}

func TestLoginHandlerNext(t *testing.T) {
	fp := newFakeProvider(t)
	h := testApp(newTestManager(t, fp.authConfig()))
	tests := map[string]string{
		"/auth/login?next=%2Frules":                   "/rules",
		"/auth/login":                                 "/",
		"/auth/login?next=%2F%2Fevil.example":         "/",
		"/auth/login?next=https%3A%2F%2Fevil.example": "/",
		"/auth/login?next=%2Fauth%2Flogin":            "/",
	}
	for target, want := range tests {
		ls := beginLogin(t, h, http.MethodGet, target)
		fp.setClaims(fp.defaultClaims(ls.nonce))
		rec := callback(t, h, ls, ls.state, testCode)
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("%s: redirect = %q, want %q", target, got, want)
		}
	}
}

func TestNonGetRedirectsHome(t *testing.T) {
	fp := newFakeProvider(t)
	h := testApp(newTestManager(t, fp.authConfig()))
	ls := beginLogin(t, h, http.MethodPost, "/api/rules")
	fp.setClaims(fp.defaultClaims(ls.nonce))
	if got := callback(t, h, ls, ls.state, testCode).Header().Get("Location"); got != "/" {
		t.Fatalf("redirect = %q", got)
	}
}

func TestAllowlistBypassesAuth(t *testing.T) {
	fp := newFakeProvider(t)
	m := newTestManager(t, fp.authConfig())
	h := m.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), func(r *http.Request) bool { return r.URL.Path == "/healthz" })
	if rec := do(t, h, http.MethodGet, "/healthz", nil); rec.Code != http.StatusTeapot {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestNilManagerMiddleware(t *testing.T) {
	var m *Manager
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	if rec := do(t, m.AuthMiddleware(next, nil), http.MethodGet, "/", nil); rec.Code != http.StatusTeapot {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestConcurrentLogins(t *testing.T) {
	fp := newFakeProvider(t)
	fp.codes = map[string]map[string]any{}
	h := testApp(newTestManager(t, fp.authConfig()))

	// Each goroutine runs a full login on a different host so the per-request
	// redirect URL differs; run with -race to catch shared-state mutation.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := fmt.Sprintf("localhost:%d", 3000+i)
			req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			loc, _ := url.Parse(rec.Header().Get("Location"))
			if got := loc.Query().Get("redirect_uri"); got != "http://"+host+"/auth/callback" {
				t.Errorf("redirect_uri = %q", got)
			}

			code := fmt.Sprintf("code-%d", i)
			claims := fp.defaultClaims(loc.Query().Get("nonce"))
			claims["sub"] = code
			fp.mu.Lock()
			fp.codes[code] = claims
			fp.mu.Unlock()

			q := url.Values{"state": {loc.Query().Get("state")}, "code": {code}}
			req = httptest.NewRequest(http.MethodGet, "/auth/callback?"+q.Encode(), nil)
			req.Host = host
			for _, c := range rec.Result().Cookies() {
				req.AddCookie(c)
			}
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Errorf("%s: callback = %d: %s", host, rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
}

func TestCallbackURL(t *testing.T) {
	fp := newFakeProvider(t)
	m := newTestManager(t, fp.authConfig())
	tests := []struct {
		host string
		tls  bool
		want string
	}{
		{"localhost:2623", false, "http://localhost:2623/auth/callback"},
		{"127.0.0.1:2623", false, "http://127.0.0.1:2623/auth/callback"},
		{"[::1]:2623", false, "http://[::1]:2623/auth/callback"},
		{"mail.example.com", false, "https://mail.example.com/auth/callback"},
		{"localhost:2623", true, "https://localhost:2623/auth/callback"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = tt.host
		if tt.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := m.callbackURL(r); got != tt.want {
			t.Errorf("%s tls=%v: %q, want %q", tt.host, tt.tls, got, tt.want)
		}
	}

	cfg := fp.authConfig()
	cfg.RedirectURL = "https://mail.example.com/auth/callback"
	m = newTestManager(t, cfg)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "evil.example"
	if got := m.callbackURL(r); got != cfg.RedirectURL {
		t.Errorf("configured redirect_url ignored: %q", got)
	}
}

func TestSafeRedirect(t *testing.T) {
	tests := map[string]string{
		"":                       "/",
		"/":                      "/",
		"/emails?page=2#x":       "/emails?page=2#x",
		"//evil.example":         "/",
		"//evil.example/path":    "/",
		`/\evil.example`:         "/",
		"https://evil.example/":  "/",
		"javascript:alert(1)":    "/",
		"evil.example":           "/",
		"/\t/evil.example":       "/",
		"/ok\r\nSet-Cookie: x=1": "/",
		"/auth/callback?code=1":  "/",
		"/auth/login":            "/",
		"/authors":               "/authors",
	}
	for in, want := range tests {
		if got := safeRedirect(in); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmailAllowed(t *testing.T) {
	domains := []string{"example.com", " Corp.Example "}
	tests := map[string]bool{
		"alice@example.com":          true,
		"ALICE@EXAMPLE.COM":          true,
		"bob@corp.example":           true,
		"eve@evil.example":           false,
		"eve@example.com.evil":       false,
		"eve@sub.example.com":        false,
		"":                           false,
		"no-at-sign":                 false,
		"@example.com":               false,
		"trailing@":                  false,
		"a@evil.example@example.com": true, // last @ wins, matching how mail is routed
	}
	for email, want := range tests {
		if got := emailAllowed(email, domains); got != want {
			t.Errorf("emailAllowed(%q) = %v, want %v", email, got, want)
		}
	}
}

func TestNewManagerConfigErrors(t *testing.T) {
	fp := newFakeProvider(t)
	base := fp.authConfig()

	if m, err := NewManager(context.Background(), models.HTTPAuthConfig{}, localHTTP); m != nil || err != nil {
		t.Fatalf("disabled: %v %v", m, err)
	}

	tests := []struct {
		name string
		mod  func(c *models.HTTPAuthConfig)
		want string
	}{
		{"no providers", func(c *models.HTTPAuthConfig) { c.Providers = nil }, "no providers"},
		{"no client id", func(c *models.HTTPAuthConfig) { c.Providers[0].ClientID = "" }, "client_id"},
		{"github", func(c *models.HTTPAuthConfig) {
			c.Providers[0] = models.HTTPAuthProviderConfig{Name: "GitHub", ClientID: "a", ClientSecret: "b"}
		}, "OpenID Connect"},
		{"unknown without issuer", func(c *models.HTTPAuthConfig) { c.Providers[0].IssuerURL = "" }, "requires issuer_url"},
		{"bad redirect url", func(c *models.HTTPAuthConfig) { c.RedirectURL = "/auth/callback" }, "redirect_url"},
		{"discovery failure", func(c *models.HTTPAuthConfig) { c.Providers[0].IssuerURL = fp.srv.URL + "/nope" }, "oidc discovery"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.Providers = append([]models.HTTPAuthProviderConfig(nil), base.Providers...)
			tt.mod(&cfg)
			_, err := NewManager(context.Background(), cfg, localHTTP)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestNewManagerOptions(t *testing.T) {
	fp := newFakeProvider(t)
	cfg := fp.authConfig()
	cfg.Providers[0].Name = ""
	cfg.Providers[0].Scopes = []string{"groups", "email", "groups"}
	cfg.CookieName = "custom"
	cfg.CookieDomain = "example.com"
	cfg.CookieSameSite = "Strict"
	m := newTestManager(t, cfg)

	if got := strings.Join(m.oauth.Scopes, " "); got != "openid email profile groups" {
		t.Fatalf("scopes = %q", got)
	}
	if m.providerName != fp.srv.URL {
		t.Fatalf("provider name = %q", m.providerName)
	}
	if m.sessionCookieName() != "custom" || m.store.Options.Domain != "example.com" {
		t.Fatalf("cookie options not applied: %+v", m.store.Options)
	}
	if m.store.Options.SameSite != http.SameSiteStrictMode || m.store.Options.Secure {
		t.Fatalf("cookie flags: %+v", m.store.Options)
	}
	// Strict would drop the state cookie on the provider's redirect back.
	if m.stateCookieSameSite() != http.SameSiteLaxMode {
		t.Fatal("state cookie must not be strict")
	}

	cfg.CookieSameSite = "none"
	if m := newTestManager(t, cfg); m.stateCookieSameSite() != http.SameSiteNoneMode {
		t.Fatal("state cookie should follow samesite=none")
	}
}

func TestShouldUseSecureCookie(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		addr     string
		override *bool
		want     bool
	}{
		{"127.0.0.1:2623", nil, false},
		{"localhost:2623", nil, false},
		{"[::1]:2623", nil, false},
		{"0.0.0.0:2623", nil, true},
		{":2623", nil, true},
		{"127.0.0.1:2623", &yes, true},
		{"0.0.0.0:2623", &no, false},
	}
	for _, tt := range tests {
		got := shouldUseSecureCookie(models.HTTPAuthConfig{CookieSecure: tt.override}, models.HTTPConfig{Addr: tt.addr})
		if got != tt.want {
			t.Errorf("%s override=%v: %v, want %v", tt.addr, tt.override, got, tt.want)
		}
	}
}

func TestGoogleDefaultIssuer(t *testing.T) {
	// Discovery against the real Google issuer is not wanted in tests; a
	// cancelled context proves the default issuer is used without network I/O.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := models.HTTPAuthConfig{Enabled: true, Providers: []models.HTTPAuthProviderConfig{{Name: "google", ClientID: "a", ClientSecret: "b"}}}
	_, err := NewManager(ctx, cfg, localHTTP)
	if err == nil || !strings.Contains(err.Error(), googleIssuer) {
		t.Fatalf("err = %v", err)
	}
}
