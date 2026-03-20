package httpcsrf

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

const (
	// CookieName is the name of the CSRF cookie.
	CookieName = "mailrelay_csrf"

	headerName = "X-CSRF-Token"
	formField  = "csrf_token"
)

// EnsureToken returns the CSRF token for this request, creating and setting
// a cookie if one does not already exist.
func EnsureToken(w http.ResponseWriter, r *http.Request, secure bool) string {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		return c.Value
	}

	token := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false, // double-submit cookie pattern; JS needs access to set header
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return token
}

// Validate reports whether the request contains a valid CSRF token that
// matches the value stored in the cookie.
func Validate(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return false
	}
	want := c.Value

	// Prefer explicit header (for htmx / AJAX).
	if h := r.Header.Get(headerName); h != "" {
		return h == want
	}

	// Fall back to form field (FormValue handles both url-encoded and multipart).
	if v := r.FormValue(formField); v != "" {
		return v == want
	}
	return false
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read should not fail; fall back to empty token which will
		// be rejected by validation rather than silently disabling CSRF.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

