package httpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/sessions"
	"github.com/gowthamgts/mailrelay/internal/models"
	"golang.org/x/oauth2"
)

// contextKey is a private type for request context keys.
type contextKey string

const userContextKey contextKey = "httpauth_user"

const (
	stateCookieName = "mailrelay_auth_state"
	stateMaxAge     = 10 * 60 // seconds
	callbackPath    = "/auth/callback"
	googleIssuer    = "https://accounts.google.com"
)

func init() {
	// Sessions are gob-encoded into the cookie; *User is stored as an
	// interface value and must be registered.
	gob.Register(&User{})
}

// User represents the authenticated HTTP user.
type User struct {
	Subject  string
	Email    string
	Name     string
	Provider string
}

// Manager wires together the OIDC provider, session storage, and middleware.
type Manager struct {
	cfg          models.HTTPAuthConfig
	httpCfg      models.HTTPConfig
	store        *sessions.CookieStore
	providerName string
	oauth        oauth2.Config
	verifier     *oidc.IDTokenVerifier
}

// NewManager constructs a Manager from the app config. When auth is disabled,
// it returns nil without error. It performs OIDC discovery against the
// provider's issuer URL, so ctx bounds that network call.
func NewManager(ctx context.Context, cfg models.HTTPAuthConfig, httpCfg models.HTTPConfig) (*Manager, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if len(cfg.Providers) == 0 {
		return nil, errors.New("http.auth.enabled is true but no providers are configured")
	}

	// For now, use the first provider as the default.
	p := cfg.Providers[0]
	if p.ClientID == "" || p.ClientSecret == "" {
		return nil, errors.New("http.auth provider requires client_id and client_secret")
	}

	issuer := strings.TrimSpace(p.IssuerURL)
	if issuer == "" {
		switch strings.ToLower(p.Name) {
		case "google":
			issuer = googleIssuer
		case "github":
			return nil, errors.New("http.auth provider \"github\" is not supported: GitHub does not implement OpenID Connect; use an OIDC provider (Google, Okta, Auth0, Keycloak, Authentik, ...)")
		default:
			return nil, fmt.Errorf("http.auth provider %q requires issuer_url", p.Name)
		}
	}

	if cfg.RedirectURL != "" {
		u, err := url.Parse(cfg.RedirectURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("http.auth.redirect_url %q must be an absolute http(s) URL", cfg.RedirectURL)
		}
	}

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", issuer, err)
	}

	oauthCfg := oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}

	// Merge additional scopes, avoiding duplicates.
	scopeSet := map[string]struct{}{}
	for _, s := range oauthCfg.Scopes {
		scopeSet[s] = struct{}{}
	}
	for _, s := range p.Scopes {
		if _, ok := scopeSet[s]; !ok {
			scopeSet[s] = struct{}{}
			oauthCfg.Scopes = append(oauthCfg.Scopes, s)
		}
	}

	// Generate a random key for cookie signing if not provided; this
	// keeps the implementation simple while still protecting against tampering.
	var key []byte
	if cfg.SessionSecret != "" {
		sum := sha256.Sum256([]byte(cfg.SessionSecret))
		key = sum[:]
	} else {
		var randKey [32]byte
		if _, err := rand.Read(randKey[:]); err != nil {
			return nil, err
		}
		key = randKey[:]
	}
	store := sessions.NewCookieStore(key)
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   int(cfg.SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   shouldUseSecureCookie(cfg, httpCfg),
		SameSite: sameSiteFromConfig(cfg.CookieSameSite),
	}
	if cfg.CookieDomain != "" {
		store.Options.Domain = cfg.CookieDomain
	}

	name := p.Name
	if name == "" {
		name = issuer
	}

	return &Manager{
		cfg:          cfg,
		httpCfg:      httpCfg,
		store:        store,
		providerName: name,
		oauth:        oauthCfg,
		verifier:     provider.Verifier(&oidc.Config{ClientID: p.ClientID}),
	}, nil
}

func sameSiteFromConfig(v string) http.SameSite {
	switch strings.ToLower(v) {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}

func shouldUseSecureCookie(cfg models.HTTPAuthConfig, httpCfg models.HTTPConfig) bool {
	if cfg.CookieSecure != nil {
		return *cfg.CookieSecure
	}
	// Default: secure for non-localhost bindings.
	return !isLoopbackHost(httpCfg.Addr)
}

// isLoopbackHost reports whether hostport (with or without a port) names
// localhost or a loopback IP.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// WithUser returns the request with the authenticated user attached to context.
func WithUser(r *http.Request, u *User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userContextKey, u))
}

// CurrentUser extracts the authenticated user from the request, if any.
func CurrentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userContextKey).(*User)
	return u
}

// AuthMiddleware enforces authentication for all requests except those matching
// the provided allowlist predicate.
func (m *Manager) AuthMiddleware(next http.Handler, allow func(*http.Request) bool) http.Handler {
	if m == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allow != nil && allow(r) {
			next.ServeHTTP(w, r)
			return
		}

		session, _ := m.store.Get(r, m.sessionCookieName())
		if session != nil {
			if v, ok := session.Values["user"].(*User); ok && v != nil {
				next.ServeHTTP(w, WithUser(r, v))
				return
			}
		}

		// Only page navigations go back to the original URL after login.
		dest := "/"
		if r.Method == http.MethodGet {
			dest = r.URL.RequestURI()
		}
		m.startLogin(w, r, dest)
	})
}

func (m *Manager) sessionCookieName() string {
	if m.cfg.CookieName != "" {
		return m.cfg.CookieName
	}
	return "mailrelay_session"
}

// oauthConfig returns a per-request copy of the OAuth2 config with the
// redirect URL filled in, so concurrent requests never share mutable state.
func (m *Manager) oauthConfig(r *http.Request) *oauth2.Config {
	c := m.oauth
	c.RedirectURL = m.callbackURL(r)
	return &c
}

func (m *Manager) callbackURL(r *http.Request) string {
	if m.cfg.RedirectURL != "" {
		return m.cfg.RedirectURL
	}
	scheme := "https"
	if r.TLS == nil && isLoopbackHost(r.Host) {
		scheme = "http"
	}
	return scheme + "://" + r.Host + callbackPath
}

// stateCookieSameSite picks the SameSite mode for the short-lived state
// cookie. It must be sent on the provider's cross-site redirect back to the
// callback, so "strict" is downgraded to "lax".
func (m *Manager) stateCookieSameSite() http.SameSite {
	if s := sameSiteFromConfig(m.cfg.CookieSameSite); s == http.SameSiteNoneMode {
		return s
	}
	return http.SameSiteLaxMode
}

// startLogin begins the OIDC authorization code flow. State, nonce and the
// post-login redirect are kept in a signed, short-lived cookie.
func (m *Manager) startLogin(w http.ResponseWriter, r *http.Request, next string) {
	state, err := randomString()
	if err != nil {
		slog.Error("failed to generate auth state", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	nonce, err := randomString()
	if err != nil {
		slog.Error("failed to generate auth nonce", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	sess, _ := m.store.New(r, stateCookieName)
	sess.Options.MaxAge = stateMaxAge
	sess.Options.SameSite = m.stateCookieSameSite()
	sess.Values["state"] = state
	sess.Values["nonce"] = nonce
	sess.Values["next"] = safeRedirect(next)
	if err := sess.Save(r, w); err != nil {
		slog.Error("failed to save auth state", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	authURL := m.oauthConfig(r).AuthCodeURL(state, oidc.Nonce(nonce))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// clearStateCookie expires the state cookie.
func (m *Manager) clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.store.Options.Secure,
		SameSite: m.stateCookieSameSite(),
	})
}

// LoginHandler is a public endpoint that explicitly triggers login. An
// optional local "next" query parameter sets where to go afterwards.
func (m *Manager) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.startLogin(w, r, r.URL.Query().Get("next"))
	})
}

// CallbackHandler completes the OIDC authorization code flow: it checks the
// state, exchanges the code, verifies the ID token and starts a session.
func (m *Manager) CallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			slog.Warn("oidc provider returned an error", "error", e, "description", q.Get("error_description"))
			m.clearStateCookie(w)
			http.Error(w, "Authentication failed", http.StatusUnauthorized)
			return
		}

		state := q.Get("state")
		code := q.Get("code")
		if state == "" || code == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		stateSess, err := m.store.Get(r, stateCookieName)
		if err != nil || stateSess.IsNew {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			return
		}
		wantState, _ := stateSess.Values["state"].(string)
		nonce, _ := stateSess.Values["nonce"].(string)
		next, _ := stateSess.Values["next"].(string)
		if wantState == "" || wantState != state || nonce == "" {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			return
		}
		// The state is single-use.
		m.clearStateCookie(w)

		ctx := r.Context()
		token, err := m.oauthConfig(r).Exchange(ctx, code)
		if err != nil {
			slog.Error("oauth2 exchange failed", "error", err)
			http.Error(w, "Authentication failed", http.StatusUnauthorized)
			return
		}

		rawIDToken, _ := token.Extra("id_token").(string)
		if rawIDToken == "" {
			slog.Error("oidc token response has no id_token")
			http.Error(w, "Authentication failed", http.StatusUnauthorized)
			return
		}

		idToken, err := m.verifier.Verify(ctx, rawIDToken)
		if err != nil {
			slog.Warn("id token verification failed", "error", err)
			http.Error(w, "Authentication failed", http.StatusUnauthorized)
			return
		}
		if idToken.Nonce != nonce {
			slog.Warn("id token nonce mismatch")
			http.Error(w, "Authentication failed", http.StatusUnauthorized)
			return
		}

		var claims struct {
			Email         string `json:"email"`
			EmailVerified *bool  `json:"email_verified"`
			Name          string `json:"name"`
		}
		if err := idToken.Claims(&claims); err != nil {
			slog.Warn("failed to parse id token claims", "error", err)
			http.Error(w, "Authentication failed", http.StatusUnauthorized)
			return
		}

		if len(m.cfg.AllowedEmailDomains) > 0 {
			verified := claims.EmailVerified != nil && *claims.EmailVerified
			if !verified || !emailAllowed(claims.Email, m.cfg.AllowedEmailDomains) {
				slog.Warn("login denied", "email", claims.Email, "email_verified", verified)
				http.Error(w, "Access denied", http.StatusForbidden)
				return
			}
		}

		user := &User{
			Subject:  idToken.Subject,
			Email:    claims.Email,
			Name:     claims.Name,
			Provider: m.providerName,
		}

		// Always start a fresh session on login.
		session, _ := m.store.New(r, m.sessionCookieName())
		session.Values["user"] = user
		if err := session.Save(r, w); err != nil {
			slog.Error("failed to save session", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		slog.Info("user logged in", "email", user.Email, "subject", user.Subject, "provider", user.Provider)
		http.Redirect(w, r, safeRedirect(next), http.StatusSeeOther)
	})
}

// LogoutHandler clears the user session and redirects to the home page.
func (m *Manager) LogoutHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := m.store.Get(r, m.sessionCookieName())
		if session != nil {
			session.Values = map[any]any{}
			session.Options.MaxAge = -1
			_ = session.Save(r, w)
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

func randomString() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func emailAllowed(email string, allowedDomains []string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return false
	}
	domain := email[at+1:]
	for _, d := range allowedDomains {
		if strings.EqualFold(strings.TrimSpace(d), domain) {
			return true
		}
	}
	return false
}

// safeRedirect returns u if it is a same-origin path, otherwise "/". It
// rejects absolute URLs and protocol-relative forms like "//host" and "/\host"
// that browsers treat as off-site.
func safeRedirect(u string) string {
	if u == "" || u[0] != '/' {
		return "/"
	}
	if len(u) > 1 && (u[1] == '/' || u[1] == '\\') {
		return "/"
	}
	if strings.ContainsAny(u, "\r\n\t") {
		return "/"
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "/"
	}
	// Never bounce back into the auth endpoints after logging in.
	if strings.HasPrefix(parsed.Path, "/auth/") {
		return "/"
	}
	return u
}
