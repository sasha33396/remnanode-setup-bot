package webconsole

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookieName = "remna_web_session"
	loginStateCookie  = "remna_login_state"
)

type session struct {
	UserID    int64  `json:"uid"`
	Name      string `json:"name"`
	ExpiresAt int64  `json:"exp"`
}

type oidcLoginAttempt struct {
	State     string `json:"state"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	ExpiresAt int64  `json:"exp"`
}

func newOIDCLoginAttempt(now time.Time) (oidcLoginAttempt, error) {
	state, err := randomToken(24)
	if err != nil {
		return oidcLoginAttempt{}, err
	}
	nonce, err := randomToken(24)
	if err != nil {
		return oidcLoginAttempt{}, err
	}
	return oidcLoginAttempt{State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier(), ExpiresAt: now.Add(10 * time.Minute).Unix()}, nil
}

func (s *Server) signOIDCLoginAttempt(value oidcLoginAttempt) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.sessionKey)
	_, _ = mac.Write([]byte("oidc:" + encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Server) readOIDCLoginAttempt(r *http.Request) (oidcLoginAttempt, error) {
	cookie, err := r.Cookie(loginStateCookie)
	if err != nil {
		return oidcLoginAttempt{}, err
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return oidcLoginAttempt{}, errors.New("invalid OIDC state cookie")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return oidcLoginAttempt{}, err
	}
	mac := hmac.New(sha256.New, s.sessionKey)
	_, _ = mac.Write([]byte("oidc:" + parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return oidcLoginAttempt{}, errors.New("invalid OIDC state signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return oidcLoginAttempt{}, err
	}
	var value oidcLoginAttempt
	if json.Unmarshal(payload, &value) != nil || value.State == "" || value.Nonce == "" || value.Verifier == "" || value.ExpiresAt <= s.now().Unix() {
		return oidcLoginAttempt{}, errors.New("expired OIDC login attempt")
	}
	return value, nil
}

func sessionFromOIDCIDToken(token *oidc.IDToken) (session, error) {
	var claims struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		GivenName  string `json:"given_name"`
		FamilyName string `json:"family_name"`
		Username   string `json:"preferred_username"`
	}
	if token == nil || token.Claims(&claims) != nil || claims.ID <= 0 {
		return session{}, errors.New("invalid Telegram OIDC claims")
	}
	name := strings.TrimSpace(claims.Name)
	if name == "" {
		name = strings.TrimSpace(claims.GivenName + " " + claims.FamilyName)
	}
	if name == "" {
		name = strings.TrimSpace(claims.Username)
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return session{UserID: claims.ID, Name: name}, nil
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *Server) issueSessionCookie(w http.ResponseWriter, value session) error {
	value.ExpiresAt = s.now().Add(s.config.SessionTTL).Unix()
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.sessionKey)
	_, _ = mac.Write([]byte(encoded))
	signed := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: signed, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: time.Unix(value.ExpiresAt, 0), MaxAge: int(s.config.SessionTTL.Seconds())})
	return nil
}

func (s *Server) readSession(r *http.Request) (session, string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return session{}, "", false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return session{}, "", false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return session{}, "", false
	}
	mac := hmac.New(sha256.New, s.sessionKey)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return session{}, "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return session{}, "", false
	}
	var value session
	if json.Unmarshal(payload, &value) != nil || value.UserID <= 0 || value.ExpiresAt <= s.now().Unix() {
		return session{}, "", false
	}
	if _, allowed := s.allowedUsers[value.UserID]; !allowed {
		return session{}, "", false
	}
	return value, cookie.Value, true
}

func (s *Server) csrfToken(sessionCookie string) string {
	mac := hmac.New(sha256.New, s.sessionKey)
	_, _ = mac.Write([]byte("csrf:" + sessionCookie))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validCSRF(r *http.Request, sessionCookie string) bool {
	received := r.Header.Get("X-CSRF-Token")
	return hmac.Equal([]byte(received), []byte(s.csrfToken(sessionCookie)))
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}
