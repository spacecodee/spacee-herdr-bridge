// Package auth implements the bridge's token file and cookie session.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CookieName is the session cookie; its value is "<device id>.<device secret>".
// A cookie equal to the master token (sessions from before devices) is
// upgraded to a device on first use.
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

// legacyReuse is how long requests still carrying the old master-token cookie
// share one device per User-Agent (the grant lives in memory), so a client that
// never stores the replacement cookie cannot mint devices and evict real ones.
const legacyReuse = 24 * time.Hour

type deviceKey struct{}

// DeviceID returns the signed-in device of a request that passed Require.
func DeviceID(ctx context.Context) string {
	id, _ := ctx.Value(deviceKey{}).(string)
	return id
}

type legacyGrant struct {
	id, secret string
	until      time.Time
}

// Auth validates the master token and per-device sessions.
type Auth struct {
	token   string
	devices *deviceStore
	now     func() time.Time
	pair    pairing

	mu     sync.Mutex // guards ctxs, hooks, legacy; taken before the store's lock
	ctxs   map[string]*devCtx
	hooks  []func(deviceID string)
	legacy map[string]legacyGrant // by User-Agent
}

type devCtx struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// New returns an Auth whose devices live in memory only.
func New(token string) *Auth {
	a, _ := Open(token, "")
	return a
}

// Open returns an Auth that keeps devices in devicesPath ("" = memory only).
func Open(token, devicesPath string) (*Auth, error) {
	st, err := openDeviceStore(devicesPath)
	if err != nil {
		return nil, err
	}
	return &Auth{
		token: token, devices: st, now: time.Now,
		ctxs: map[string]*devCtx{}, legacy: map[string]legacyGrant{},
	}, nil
}

// OnRevoke registers fn to run after a device is signed out or evicted.
func (a *Auth) OnRevoke(fn func(deviceID string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hooks = append(a.hooks, fn)
}

func (a *Auth) valid(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(a.token)) == 1
}

// createDevice stores a new device; a.mu must be held. It returns the cookie
// value and the ids evicted (already cancelled; the caller runs finishRevoke).
func (a *Auth) createDevice(ua string) (value string, evicted []string, err error) {
	var idb [16]byte
	var secb [32]byte
	for _, b := range [][]byte{idb[:], secb[:]} {
		if _, err := rand.Read(b); err != nil {
			return "", nil, err
		}
	}
	id := hex.EncodeToString(idb[:])
	secret := base64.RawURLEncoding.EncodeToString(secb[:])
	sum := sha256.Sum256([]byte(secret))
	platform := platformFromUA(ua)
	now := a.now()
	evicted, err = a.devices.add(Device{
		ID: id, Name: defaultName(platform), Platform: platform,
		Created: now, LastSeen: now, SecretHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return "", nil, err
	}
	for _, e := range evicted {
		a.cancelLocked(e)
	}
	return id + "." + secret, evicted, nil
}

func (a *Auth) cancelLocked(id string) {
	if d := a.ctxs[id]; d != nil {
		d.cancel()
		delete(a.ctxs, id)
	}
	for ua, g := range a.legacy {
		if g.id == id {
			delete(a.legacy, ua)
		}
	}
}

func (a *Auth) finishRevoke(ids []string) {
	a.mu.Lock()
	hooks := append([]func(string){}, a.hooks...)
	a.mu.Unlock()
	for _, id := range ids {
		for _, h := range hooks {
			h(id)
		}
	}
}

// Revoke signs a device out: it is removed, its open requests are cancelled
// and the OnRevoke hooks run. It reports whether the device existed.
func (a *Auth) Revoke(id string) bool {
	a.mu.Lock()
	ok, err := a.devices.remove(id)
	if err != nil {
		log.Printf("auth: could not save devices: %v", err)
	}
	if ok {
		a.cancelLocked(id)
	}
	a.mu.Unlock()
	if ok {
		a.finishRevoke([]string{id})
	}
	return ok
}

// authenticate resolves the request's cookie to a device. upgrade is a
// replacement cookie to send when an old master-token cookie was upgraded.
func (a *Auth) authenticate(r *http.Request) (id string, dctx context.Context, upgrade *http.Cookie, ok bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return "", nil, nil, false
	}
	if a.valid(c.Value) {
		return a.upgradeLegacy(r)
	}
	id, secret, found := strings.Cut(c.Value, ".")
	if !found {
		return "", nil, nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.devices.verify(id, secret, a.now()); !ok {
		return "", nil, nil, false
	}
	return id, a.ctxLocked(id), nil, true
}

func (a *Auth) ctxLocked(id string) context.Context {
	d := a.ctxs[id]
	if d == nil {
		ctx, cancel := context.WithCancel(context.Background())
		d = &devCtx{ctx, cancel}
		a.ctxs[id] = d
	}
	return d.ctx
}

func (a *Auth) upgradeLegacy(r *http.Request) (string, context.Context, *http.Cookie, bool) {
	ua := r.UserAgent()
	a.mu.Lock()
	now := a.now()
	g, reuse := a.legacy[ua]
	if !reuse || now.After(g.until) {
		value, evicted, err := a.createDevice(ua)
		if err != nil {
			a.mu.Unlock()
			log.Printf("auth: could not upgrade session: %v", err)
			return "", nil, nil, false
		}
		id, secret, _ := strings.Cut(value, ".")
		g = legacyGrant{id: id, secret: secret, until: now.Add(legacyReuse)}
		a.legacy[ua] = g
		a.mu.Unlock()
		a.finishRevoke(evicted)
		a.mu.Lock()
		if !a.devices.has(g.id) { // signed out while the lock was released
			a.mu.Unlock()
			return "", nil, nil, false
		}
	}
	ctx := a.ctxLocked(g.id)
	a.mu.Unlock()
	return g.id, ctx, a.cookie(g.id+"."+g.secret, 31536000), true
}

func (a *Auth) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
}

// Require rejects requests without a valid session cookie. The request context
// carries the device id (DeviceID) and is cancelled when the device is signed out.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, dctx, upgrade, ok := a.authenticate(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if upgrade != nil {
			http.SetCookie(w, upgrade)
		}
		ctx, cancel := context.WithCancel(context.WithValue(r.Context(), deviceKey{}, id))
		defer cancel()
		defer context.AfterFunc(dctx, cancel)()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SessionHandler serves GET (cookie check), POST (login) and DELETE (sign out) on /api/session.
func (a *Auth) SessionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _, upgrade, ok := a.authenticate(r)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if upgrade != nil {
				http.SetCookie(w, upgrade)
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			a.login(w, r)
		case http.MethodDelete:
			a.logout(w, r)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
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
		Code  string `json:"code"`
	}
	if err != nil || json.Unmarshal(body, &req) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case req.Token != "":
		if !a.valid(req.Token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	case req.Code != "":
		switch a.pair.redeem(a.now(), req.Code) {
		case redeemLocked:
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		case redeemInvalid:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	default:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	a.mu.Lock()
	value, evicted, err := a.createDevice(r.UserAgent())
	a.mu.Unlock()
	if err != nil {
		log.Printf("auth: could not create device: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	a.finishRevoke(evicted)
	http.SetCookie(w, a.cookie(value, 31536000))
	w.WriteHeader(http.StatusNoContent)
}

// logout signs the current device out and clears the cookie; it is idempotent.
func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if !SameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(CookieName); err == nil && !a.valid(c.Value) {
		if id, _, ok := strings.Cut(c.Value, "."); ok {
			// Only the device that owns the secret may sign itself out.
			if _, _, _, ok := a.authenticate(r); ok {
				a.Revoke(id)
			}
		}
	}
	http.SetCookie(w, a.cookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}
