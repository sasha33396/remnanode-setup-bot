package webconsole

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
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

func verifyTelegramLogin(values url.Values, botToken string, now time.Time) (session, error) {
	hashValue := values.Get("hash")
	if len(hashValue) != sha256.Size*2 {
		return session{}, errors.New("missing Telegram authorization hash")
	}
	keys := make([]string, 0, len(values))
	for key, entries := range values {
		if key == "hash" || key == "state" {
			continue
		}
		if len(entries) != 1 || strings.ContainsAny(key, "\r\n=") || strings.ContainsAny(entries[0], "\r\n") {
			return session{}, errors.New("invalid Telegram authorization data")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values.Get(key))
	}
	secret := sha256.Sum256([]byte(botToken))
	mac := hmac.New(sha256.New, secret[:])
	_, _ = mac.Write([]byte(strings.Join(parts, "\n")))
	expected, err := hex.DecodeString(hashValue)
	if err != nil || !hmac.Equal(mac.Sum(nil), expected) {
		return session{}, errors.New("invalid Telegram authorization signature")
	}

	authDate, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil || authDate > now.Add(time.Minute).Unix() || now.Sub(time.Unix(authDate, 0)) > 5*time.Minute {
		return session{}, errors.New("expired Telegram authorization")
	}
	userID, err := strconv.ParseInt(values.Get("id"), 10, 64)
	if err != nil || userID <= 0 {
		return session{}, errors.New("invalid Telegram user")
	}
	name := strings.TrimSpace(values.Get("first_name") + " " + values.Get("last_name"))
	if name == "" {
		name = strings.TrimSpace(values.Get("username"))
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return session{UserID: userID, Name: name}, nil
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
