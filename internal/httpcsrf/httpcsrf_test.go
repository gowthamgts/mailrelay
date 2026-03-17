package httpcsrf

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnsureToken_ReturnsExistingCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "existing-token"})

	w := httptest.NewRecorder()
	token := EnsureToken(w, r, false)

	if token != "existing-token" {
		t.Errorf("token = %q, want existing-token", token)
	}
	// No new Set-Cookie should be sent when an existing cookie is present.
	if w.Result().Cookies() != nil {
		for _, c := range w.Result().Cookies() {
			if c.Name == CookieName {
				t.Error("unexpected new Set-Cookie when cookie already exists")
			}
		}
	}
}

func TestEnsureToken_CreatesCookieWhenMissing(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	token := EnsureToken(w, r, false)

	if token == "" {
		t.Fatal("expected non-empty token")
	}
	cookies := w.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == CookieName {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("expected Set-Cookie header for CSRF token")
	}
	if found.Value != token {
		t.Errorf("cookie value = %q, want %q", found.Value, token)
	}
	if found.Secure {
		t.Error("expected Secure=false when secure=false")
	}
	if found.HttpOnly {
		t.Error("CSRF cookie must not be HttpOnly (double-submit pattern)")
	}
}

func TestEnsureToken_SecureCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	EnsureToken(w, r, true)

	cookies := w.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == CookieName {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("expected Set-Cookie header")
	}
	if !found.Secure {
		t.Error("expected Secure=true when secure=true")
	}
}

func TestValidate_WithHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "secret-token"})
	r.Header.Set("X-CSRF-Token", "secret-token")

	if !Validate(r) {
		t.Error("expected Validate to return true with matching header")
	}
}

func TestValidate_WithFormField(t *testing.T) {
	body := strings.NewReader("csrf_token=form-token")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "form-token"})

	if !Validate(r) {
		t.Error("expected Validate to return true with matching form field")
	}
}

func TestValidate_HeaderTakesPriorityOverForm(t *testing.T) {
	body := strings.NewReader("csrf_token=wrong-token")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "correct-token"})
	r.Header.Set("X-CSRF-Token", "correct-token")

	if !Validate(r) {
		t.Error("expected Validate to return true when header matches")
	}
}

func TestValidate_NoCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-CSRF-Token", "some-token")

	if Validate(r) {
		t.Error("expected Validate to return false when cookie is missing")
	}
}

func TestValidate_MismatchHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "correct"})
	r.Header.Set("X-CSRF-Token", "wrong")

	if Validate(r) {
		t.Error("expected Validate to return false for mismatched header token")
	}
}

func TestValidate_MismatchFormField(t *testing.T) {
	body := strings.NewReader("csrf_token=wrong")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "correct"})

	if Validate(r) {
		t.Error("expected Validate to return false for mismatched form token")
	}
}

func TestValidate_EmptyCookieValue(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: ""})
	r.Header.Set("X-CSRF-Token", "")

	if Validate(r) {
		t.Error("expected Validate to return false for empty cookie value")
	}
}

func TestValidate_NoHeaderOrForm(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "token"})

	if Validate(r) {
		t.Error("expected Validate to return false when neither header nor form field provided")
	}
}

func TestRandomToken_IsNonEmpty(t *testing.T) {
	token := randomToken()
	if token == "" {
		t.Error("expected non-empty random token")
	}
}

func TestRandomToken_IsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		token := randomToken()
		if seen[token] {
			t.Errorf("duplicate token generated: %q", token)
		}
		seen[token] = true
	}
}

func TestRandomToken_HasExpectedLength(t *testing.T) {
	token := randomToken()
	// 32 bytes base64url-encoded without padding = 43 chars.
	if len(token) != 43 {
		t.Errorf("token length = %d, want 43", len(token))
	}
}
