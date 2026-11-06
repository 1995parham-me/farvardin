package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// CookieName is the session cookie.
const CookieName = "farvardin_session"

// Login limits: this many failures from one address within the window
// lock that address out until the window passes.
const (
	maxFailures   = 5
	failureWindow = 15 * time.Minute
)

// Errors of the session layer.
var (
	ErrBadCredentials = errors.New("wrong user name or password")
	ErrLocked         = errors.New("too many failed logins; try again later")
	ErrNoSession      = errors.New("no valid session")
)

// SessionOptions configures the login.
type SessionOptions struct {
	// Users maps user names to bcrypt hashes.
	Users map[string]string
	// SecretFile holds the HMAC key; it is created (mode 0600) when missing.
	SecretFile string
	// TTL is how long a session lasts.
	TTL time.Duration
	// Secure marks the cookie Secure (HTTPS only).
	Secure bool
	// CookiePath scopes the cookie, normally the UI prefix.
	CookiePath string
}

// Sessions issues and checks JWT sessions.
type Sessions struct {
	opts   SessionOptions
	secret []byte

	mu       sync.Mutex
	failures map[string][]time.Time
}

type claims struct {
	jwt.RegisteredClaims

	// PasswordTag ties the session to the password hash it was issued
	// for: changing a password ends every older session.
	PasswordTag string `json:"pwt"`
}

const issuer = "farvardin"

// NewSessions loads (or creates) the signing key.
func NewSessions(opts SessionOptions) (*Sessions, error) {
	secret, err := os.ReadFile(opts.SecretFile)
	if errors.Is(err, os.ErrNotExist) {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("session key: %w", err)
		}

		secret = []byte(hex.EncodeToString(secret))

		if err := os.MkdirAll(filepath.Dir(opts.SecretFile), 0o700); err != nil {
			return nil, fmt.Errorf("session key: %w", err)
		}

		if err := os.WriteFile(opts.SecretFile, secret, 0o600); err != nil {
			return nil, fmt.Errorf("session key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}

	secret = []byte(strings.TrimSpace(string(secret)))
	if len(secret) < 32 {
		return nil, fmt.Errorf("session key in %s is shorter than 32 bytes", opts.SecretFile) //nolint:err113 // operator-facing
	}

	if opts.TTL <= 0 {
		opts.TTL = 7 * 24 * time.Hour
	}

	return &Sessions{opts: opts, secret: secret, mu: sync.Mutex{}, failures: map[string][]time.Time{}}, nil
}

func passwordTag(hash string) string {
	sum := sha256.Sum256([]byte(hash))

	return hex.EncodeToString(sum[:6])
}

// clientIP is the peer address; behind a reverse proxy on loopback it is
// the X-Real-IP the proxy sets.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if forwarded := strings.TrimSpace(r.Header.Get("X-Real-IP")); forwarded != "" {
			return forwarded
		}
	}

	return host
}

func (s *Sessions) recentFailures(ip string, now time.Time) int {
	kept := s.failures[ip][:0]

	for _, t := range s.failures[ip] {
		if now.Sub(t) < failureWindow {
			kept = append(kept, t)
		}
	}

	if len(kept) == 0 {
		delete(s.failures, ip)
	} else {
		s.failures[ip] = kept
	}

	return len(kept)
}

// Login checks credentials and returns a signed token.
func (s *Sessions) Login(r *http.Request, user, password string) (string, error) {
	ip := clientIP(r)
	now := time.Now()

	s.mu.Lock()
	locked := s.recentFailures(ip, now) >= maxFailures
	s.mu.Unlock()

	if locked {
		return "", ErrLocked
	}

	hash, ok := s.opts.Users[user]
	if !ok {
		// Same cost as a real check, so user names do not leak.
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
	}

	if !ok || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.mu.Lock()
		s.failures[ip] = append(s.failures[ip], now)
		s.mu.Unlock()

		return "", ErrBadCredentials
	}

	s.mu.Lock()
	delete(s.failures, ip)
	s.mu.Unlock()

	id := make([]byte, 12)
	_, _ = rand.Read(id)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Issuer:      issuer,
		Subject:     user,
		ExpiresAt:   jwt.NewNumericDate(now.Add(s.opts.TTL)),
		IssuedAt:    jwt.NewNumericDate(now),
		ID:          hex.EncodeToString(id),
		PasswordTag: passwordTag(hash),
	})

	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("signing session: %w", err)
	}

	return signed, nil
}

// User returns the user of a request's session.
func (s *Sessions) User(r *http.Request) (string, error) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return "", ErrNoSession
	}

	var cl claims

	_, err = jwt.ParseWithClaims(c.Value, &cl, func(*jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return "", ErrNoSession
	}

	hash, ok := s.opts.Users[cl.Subject]
	if !ok || cl.PasswordTag != passwordTag(hash) {
		return "", ErrNoSession
	}

	return cl.Subject, nil
}

// SetCookie stores the token in the session cookie.
func (s *Sessions) SetCookie(w http.ResponseWriter, token string) {
	//nolint:gosec // Secure is configurable only so plain-HTTP tests can sign in; HttpOnly and SameSite are fixed
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: s.opts.CookiePath,
		MaxAge: int(s.opts.TTL / time.Second), HttpOnly: true, Secure: s.opts.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie ends the session in the browser.
func (s *Sessions) ClearCookie(w http.ResponseWriter) {
	//nolint:gosec // as in SetCookie
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: s.opts.CookiePath, MaxAge: -1,
		HttpOnly: true, Secure: s.opts.Secure, SameSite: http.SameSiteLaxMode,
	})
}

var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("farvardin"), bcrypt.DefaultCost)

	return h
})

type userKey struct{}

// withUser stores the signed-in user in the request context.
func withUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}

func userFrom(ctx context.Context) string {
	u, _ := ctx.Value(userKey{}).(string)

	return u
}
