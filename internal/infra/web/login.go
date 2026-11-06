package web

import (
	"errors"
	"net/http"
	"strings"
)

// safeNext keeps a post-login redirect inside the UI: a path under the
// prefix, never a scheme-relative "//host" URL.
func (h *Handler) safeNext(next string) string {
	if strings.HasPrefix(next, h.prefix+"/") && !strings.HasPrefix(next, "//") && !strings.Contains(next, "\\") {
		return next
	}

	return h.prefix + "/"
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)

		return
	}

	if _, err := h.sessions.User(r); err == nil {
		http.Redirect(w, r, h.safeNext(r.URL.Query().Get("next")), http.StatusSeeOther) //nolint:gosec // safeNext keeps the target under the UI prefix

		return
	}

	h.renderLogin(w, r, http.StatusOK, r.URL.Query().Get("next"), "", "")
}

func (h *Handler) renderLogin(w http.ResponseWriter, r *http.Request, status int, next, user, message string) {
	w.Header().Set("Cache-Control", "no-store")
	h.renderStatus(w, r, status, "login.html", map[string]any{"Title": "Sign in", "Next": h.safeNext(next), "Username": user, "Error": message})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		http.NotFound(w, r)

		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)

		return
	}

	user := strings.TrimSpace(r.PostFormValue("username"))
	next := r.PostFormValue("next")

	token, err := h.sessions.Login(r, user, r.PostFormValue("password"))

	switch {
	case errors.Is(err, ErrLocked):
		h.log.Warn("login locked out", "ip", clientIP(r), "user", user)
		h.renderLogin(w, r, http.StatusTooManyRequests, next, user, err.Error())
	case err != nil:
		// 401, so the reverse proxy's fail2ban jail counts it too.
		h.log.Warn("login failed", "ip", clientIP(r), "user", user)
		h.renderLogin(w, r, http.StatusUnauthorized, next, user, ErrBadCredentials.Error())
	default:
		h.log.Info("login", "ip", clientIP(r), "user", user)
		h.sessions.SetCookie(w, token)
		http.Redirect(w, r, h.safeNext(next), http.StatusSeeOther) //nolint:gosec // safeNext keeps the target under the UI prefix
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if h.sessions != nil {
		h.sessions.ClearCookie(w)
	}

	http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
}
