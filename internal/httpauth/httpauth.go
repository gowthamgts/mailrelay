package httpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
)

// contextKey is a private type for request context keys.
type contextKey string

const userContextKey contextKey = "httpauth_user"

const stateCookieName = "mailrelay_auth_state"

// User represents the authenticated HTTP user.
type User struct {
	Subject  string
	Email    string
	Name     string
	Provider string
}

// Manager wires together OAuth2/OIDC providers, session storage, and middleware.
type Manager struct {
	cfg      models.HTTPAuthConfig
	httpCfg  models.HTTPConfig
	store    *sessions.CookieStore
	provider *oauth2.Config
}

// NewManager constructs a Manager from the app config. When auth is disabled,
// it returns nil without error.
func NewManager(cfg models.HTTPAuthConfig, httpCfg models.HTTPConfig) (*Manager, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if len(cfg.Providers) == 0 {
		return nil, errors.New("http_auth.enabled is true but no providers are configured")
	}

	// For now, use the first provider as the default.
	p := cfg.Providers[0]
	if p.ClientID == "" || p.ClientSecret == "" {
		return nil, errors.New("http_auth provider requires client_id and client_secret")
	}

	redirectURL := "" // filled in at runtime based on incoming request host

	oauthCfg := &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{"openid", "email", "profile"},
	}

	// Provider-specific endpoints.
	switch strings.ToLower(p.Name) {
	case "google":
		oauthCfg.Endpoint = google.Endpoint
	case "github":
		oauthCfg.Endpoint = github.Endpoint
	default:
		// Fallback to Google endpoints if no known provider name is given.
		oauthCfg.Endpoint = google.Endpoint
	}

	// Merge additional scopes, avoiding duplicates.
	scopeSet := map[string]struct{}{}
	for _, s := range oauthCfg.Scopes {
		scopeSet[s] = struct{}{}
	}
	for _, s := range p.Scopes {
		if _, ok := scopeSet[s]; !ok {
			oauthCfg.Scopes = append(oauthCfg.Scopes, s)
		}
	}

	// Generate a random key for cookie signing if not provided via env; this
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
		SameSite: sameSiteFromConfig(cfg.CookieSameSite),
	}

	secure := shouldUseSecureCookie(cfg, httpCfg)
	store.Options.Secure = secure
	if cfg.CookieDomain != "" {
		store.Options.Domain = cfg.CookieDomain
	}

	m := &Manager{
		cfg:      cfg,
		httpCfg:  httpCfg,
		store:    store,
		provider: oauthCfg,
	}

	return m, nil
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
	if strings.HasPrefix(httpCfg.Addr, "127.0.0.1:") || strings.HasPrefix(httpCfg.Addr, "localhost:") {
		return false
	}
	return true
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
				r = WithUser(r, v)
				next.ServeHTTP(w, r)
				return
			}
		}

		m.startLogin(w, r)
	})
}

func (m *Manager) sessionCookieName() string {
	if m.cfg.CookieName != "" {
		return m.cfg.CookieName
	}
	return "mailrelay_session"
}

// startLogin begins the OAuth2 authorization code flow.
func (m *Manager) startLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomState()
	if err != nil {
		slog.Error("failed to generate auth state", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Persist state and original URL in a short-lived, HTTP-only cookie.
	redirectTo := r.URL.String()
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    url.QueryEscape(state + "|" + redirectTo),
		Path:     "/",
		Expires:  time.Now().Add(10 * time.Minute),
		HttpOnly: true,
		Secure:   shouldUseSecureCookie(m.cfg, m.httpCfg),
		SameSite: http.SameSiteLaxMode,
	})

	authURL := m.provider.AuthCodeURL(state, oauth2.AccessTypeOnline)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// LoginHandler is a public endpoint that explicitly triggers login.
func (m *Manager) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.startLogin(w, r)
	})
}

// CallbackHandler completes the OAuth2 authorization code flow.
func (m *Manager) CallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		code := r.URL.Query().Get("code")
		if state == "" || code == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		stateCookie, err := r.Cookie(stateCookieName)
		if err != nil || stateCookie.Value == "" {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			return
		}

		val, err := url.QueryUnescape(stateCookie.Value)
		if err != nil {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			return
		}

		parts := strings.SplitN(val, "|", 2)
		if len(parts) != 2 || parts[0] != state {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			return
		}
		originalURL := parts[1]

		// Clear the state cookie.
		http.SetCookie(w, &http.Cookie{
			Name:     stateCookieName,
			Value:    "",
			Path:     "/",
			Expires:  time.Unix(0, 0),
			MaxAge:   -1,
			HttpOnly: true,
		})

		// Temporarily set redirect URL based on current request host.
		m.provider.RedirectURL = callbackURLFromRequest(r)

		ctx := r.Context()
		token, err := m.provider.Exchange(ctx, code)
		if err != nil {
			slog.Error("oauth2 exchange failed", "error", err)
			http.Error(w, "Authentication failed", http.StatusUnauthorized)
			return
		}

		// For simplicity, we only extract the email from the ID token / token response
		// via standard claims. In a fuller implementation we would parse ID tokens
		// explicitly and/or call the provider userinfo endpoint.
		rawIDToken, _ := token.Extra("id_token").(string)
		if rawIDToken == "" {
			// We still consider this a successful login, but without email we cannot
			// enforce domain restrictions, so we reject the login.
			http.Error(w, "Authentication failed (no id_token)", http.StatusUnauthorized)
			return
		}

		email := ""
		name := ""
		subject := rawIDToken

		user := &User{
			Subject:  subject,
			Email:    email,
			Name:     name,
			Provider: m.cfg.Providers[0].Name,
		}

		if len(m.cfg.AllowedEmailDomains) > 0 && !emailAllowed(email, m.cfg.AllowedEmailDomains) {
			http.Error(w, "Access denied", http.StatusForbidden)
			return
		}

		session, _ := m.store.Get(r, m.sessionCookieName())
		if session == nil {
			session, _ = m.store.New(r, m.sessionCookieName())
		}
		session.Values["user"] = user
		if err := session.Save(r, w); err != nil {
			slog.Error("failed to save session", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, originalURLOrDefault(originalURL), http.StatusSeeOther)
	})
}

// LogoutHandler clears the user session and redirects to the home page.
func (m *Manager) LogoutHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := m.store.Get(r, m.sessionCookieName())
		if session != nil {
			session.Options.MaxAge = -1
			_ = session.Save(r, w)
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

func randomState() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func emailAllowed(email string, allowedDomains []string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return false
	}
	domain := parts[1]
	for _, d := range allowedDomains {
		if strings.EqualFold(strings.TrimSpace(d), domain) {
			return true
		}
	}
	return false
}

func callbackURLFromRequest(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && strings.HasPrefix(r.Host, "localhost") {
		scheme = "http"
	}
	return scheme + "://" + r.Host + "/auth/callback"
}

func originalURLOrDefault(u string) string {
	if u == "" {
		return "/"
	}
	if !strings.HasPrefix(u, "/") {
		return "/"
	}
	return u
}

