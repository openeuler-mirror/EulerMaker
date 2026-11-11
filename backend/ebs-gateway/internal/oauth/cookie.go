package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ebs-gateway/internal/iam"
)

const contextTTL = 10 * time.Minute

type profile struct {
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
}

type browserContext struct {
	Purpose           string                `json:"purpose"`
	Provider          string                `json:"provider"`
	ConfigFingerprint string                `json:"configFingerprint"`
	IssuedAt          int64                 `json:"issuedAt"`
	ExpiresAt         int64                 `json:"expiresAt"`
	ReturnTo          string                `json:"returnTo"`
	StateHash         string                `json:"stateHash,omitempty"`
	Verifier          string                `json:"verifier,omitempty"`
	NonceHash         string                `json:"nonceHash,omitempty"`
	RegistrationID    string                `json:"registrationID,omitempty"`
	Identity          *iam.ExternalIdentity `json:"identity,omitempty"`
	Profile           profile               `json:"profile"`
}

func randomValue() string {
	return base64.RawURLEncoding.EncodeToString(randBytes(32))
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("secure random source unavailable")
	}
	return b
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func matches(raw, hashed string) bool {
	return subtle.ConstantTimeCompare([]byte(digest(raw)), []byte(hashed)) == 1
}

func cookieName(purpose string) string { return "__Host-ebs-oauth-" + purpose }

func (h *Handler) aad(purpose string) []byte {
	return []byte("v1\x00" + cookieName(purpose) + "\x00" + purpose + "\x00" + h.config.PublicBaseURL)
}

func (h *Handler) aead(keyID string) (cipher.AEAD, error) {
	key, err := base64.StdEncoding.DecodeString(h.config.ContextKeys.Keys[keyID])
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("unknown Cookie key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (h *Handler) encodeCookie(state browserContext) (*http.Cookie, error) {
	if !h.now().Before(time.Unix(state.ExpiresAt, 0)) {
		return nil, failure(410, "ContextExpired")
	}
	aead, err := h.aead(h.config.ContextKeys.ActiveKeyID)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	nonce := randBytes(aead.NonceSize())
	sealed := aead.Seal(nonce, nonce, data, h.aad(state.Purpose))
	value := "v1." + h.config.ContextKeys.ActiveKeyID + "." + base64.RawURLEncoding.EncodeToString(sealed)
	cookie := &http.Cookie{Name: cookieName(state.Purpose), Value: value, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Expires: time.Unix(state.ExpiresAt, 0), MaxAge: int(time.Unix(state.ExpiresAt, 0).Sub(h.now()).Seconds())}
	if len(cookie.String()) > 3800 {
		if state.Profile != (profile{}) {
			state.Profile = profile{}
			return h.encodeCookie(state)
		}
		return nil, failure(400, "OAuthContextTooLarge")
	}
	return cookie, nil
}

func (h *Handler) readCookie(r *http.Request, purpose string) (browserContext, error) {
	var state browserContext
	var cookies []*http.Cookie
	for _, cookie := range r.Cookies() {
		if cookie.Name == cookieName(purpose) {
			cookies = append(cookies, cookie)
		}
	}
	if len(cookies) != 1 || len(cookies[0].Value) > 3800 {
		return state, failure(403, "BrowserBindingInvalid")
	}
	parts := strings.Split(cookies[0].Value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return state, failure(403, "BrowserBindingInvalid")
	}
	aead, err := h.aead(parts[1])
	if err != nil {
		return state, failure(403, "BrowserBindingInvalid")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sealed) < aead.NonceSize() {
		return state, failure(403, "BrowserBindingInvalid")
	}
	data, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], h.aad(purpose))
	if err != nil || json.Unmarshal(data, &state) != nil || state.Purpose != purpose || state.Provider != h.config.Provider.ID {
		return state, failure(403, "BrowserBindingInvalid")
	}
	if state.ConfigFingerprint != h.config.fingerprint() {
		return state, failure(409, "OAuthConfigChanged")
	}
	if state.IssuedAt > h.now().Unix() || state.ExpiresAt <= state.IssuedAt || state.ExpiresAt-state.IssuedAt > int64(contextTTL.Seconds()) || !h.now().Before(time.Unix(state.ExpiresAt, 0)) {
		return state, failure(410, "ContextExpired")
	}
	if !validReturnTo(state.ReturnTo) {
		return state, failure(403, "BrowserBindingInvalid")
	}
	if purpose == "login" && (state.Verifier == "" || state.StateHash == "" || state.NonceHash == "") {
		return state, failure(403, "BrowserBindingInvalid")
	}
	if purpose == "register" && (state.RegistrationID == "" || state.Identity == nil || !validIdentity(*state.Identity)) {
		return state, failure(403, "BrowserBindingInvalid")
	}
	return state, nil
}

func clearCookie(w http.ResponseWriter, purpose string) {
	http.SetCookie(w, &http.Cookie{Name: cookieName(purpose), Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}
