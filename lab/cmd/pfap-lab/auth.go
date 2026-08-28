package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type authenticator struct{ password, token string }

func newAuthenticator(password string) *authenticator {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return &authenticator{password: password, token: hex.EncodeToString(b)}
}

func (a *authenticator) valid(r *http.Request) bool {
	c, err := r.Cookie("pfap_lab_session")
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.token)) == 1
}
func authJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *authenticator) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/auth/login":
			if r.Method != "POST" {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var input struct {
				Password string `json:"password"`
			}
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input)
			if subtle.ConstantTimeCompare([]byte(input.Password), []byte(a.password)) != 1 {
				time.Sleep(250 * time.Millisecond)
				authJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "pfap_lab_session", Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
			authJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		case "/auth/logout":
			http.SetCookie(w, &http.Cookie{Name: "pfap_lab_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
			authJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		if a.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			authJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		login, err := web.ReadFile("web/login.html")
		if err != nil {
			http.Error(w, "login unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(login)
	})
}
