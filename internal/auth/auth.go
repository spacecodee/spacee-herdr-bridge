// Package auth implements the bridge's token file and cookie session.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// CookieName is the session cookie; its value is the token itself.
const CookieName = "spacee_session"

const maxLoginBody = 4 << 10

// DefaultTokenPath returns $XDG_CONFIG_HOME/spacee-herdr/token, or
// ~/.config/spacee-herdr/token (also on macOS).
func DefaultTokenPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "spacee-herdr", "token")
}

// LoadOrCreateToken reads the token at path, or creates it atomically with
// mode 0600 when the file does not exist.
func LoadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("token file %s is empty", path)
		}
		return tok, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) { // lost a creation race: use the winner's token
		return LoadOrCreateToken(path)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		return "", err
	}
	return tok, f.Close()
}

// Auth validates the shared token.
type Auth struct{ token string }

func New(token string) *Auth { return &Auth{token: token} }

func (a *Auth) valid(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(a.token)) == 1
}

func (a *Auth) hasValidCookie(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	return err == nil && a.valid(c.Value)
}

// Require rejects requests without a valid session cookie.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hasValidCookie(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SessionHandler serves GET (cookie check) and POST (login) on /api/session.
func (a *Auth) SessionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if a.hasValidCookie(r) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case http.MethodPost:
			a.login(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// SameOrigin reports whether the request has no Origin header or one whose
// host is the host being requested.
func SameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if !SameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginBody))
	var req struct {
		Token string `json:"token"`
	}
	if err != nil || json.Unmarshal(body, &req) != nil || !a.valid(req.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    a.token,
		Path:     "/",
		MaxAge:   31536000,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
