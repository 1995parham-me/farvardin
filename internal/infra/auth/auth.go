// Package auth authenticates HTTP requests with bcrypt basic auth or a
// header set by a trusted reverse proxy.
package auth

import (
	"crypto/sha256"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Middleware wraps handlers with authentication.
type Middleware struct {
	mode   string
	realm  string
	header string
	users  map[string]string

	mu       sync.Mutex
	verified map[[32]byte]time.Time
}

// New builds the middleware. mode is "none", "basic" or "header".
func New(mode, realm, header string, users map[string]string) *Middleware {
	return &Middleware{mode: mode, realm: realm, header: header, users: users, mu: sync.Mutex{}, verified: map[[32]byte]time.Time{}}
}

// verifiedFor is how long a checked password is remembered. bcrypt costs
// tens of milliseconds on a Raspberry Pi, and a sync sends many requests.
const verifiedFor = 10 * time.Minute

// Wrap returns h behind authentication.
func (m *Middleware) Wrap(h http.Handler) http.Handler {
	if m.mode == "none" {
		return h
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.allowed(r) {
			h.ServeHTTP(w, r)

			return
		}

		if m.mode == "basic" {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+m.realm+`", charset="UTF-8"`)
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (m *Middleware) allowed(r *http.Request) bool {
	if m.mode == "header" {
		return r.Header.Get(m.header) != ""
	}

	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}

	hash, known := m.users[user]
	if !known {
		// Spend the same time as a real check so user names do not leak.
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(pass))

		return false
	}

	key := sha256.Sum256([]byte(user + "\x00" + pass + "\x00" + hash))

	m.mu.Lock()
	until, cached := m.verified[key]
	m.mu.Unlock()

	if cached && time.Now().Before(until) {
		return true
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
		return false
	}

	m.mu.Lock()
	if len(m.verified) > 1024 {
		clear(m.verified)
	}

	m.verified[key] = time.Now().Add(verifiedFor)
	m.mu.Unlock()

	return true
}

// dummyHash is compared against for unknown users.
var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("farvardin"), bcrypt.DefaultCost)

	return h
})

// Hash returns a bcrypt hash for the [auth.users] table.
func Hash(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err //nolint:wrapcheck // the bcrypt message is clear on its own
	}

	return string(h), nil
}
