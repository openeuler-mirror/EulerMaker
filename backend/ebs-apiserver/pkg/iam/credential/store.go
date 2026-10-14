package credential

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"ebs-apiserver/pkg/storage/es"
)

const (
	resourceName = "user"
	memoryKiB    = 19456
	iterations   = 2
	parallelism  = 1
	saltLength   = 16
	keyLength    = 32
	maxFailures  = 5
	lockDuration = 15 * time.Minute
)

var ErrInvalidPassword = errors.New("password must contain 12 to 128 characters")
var ErrUserNotFound = errors.New("user not found")

type Credential struct {
	PasswordHash      string     `json:"passwordHash"`
	PasswordUpdatedAt time.Time  `json:"passwordUpdatedAt"`
	FailedAttempts    int        `json:"failedAttempts"`
	LockedUntil       *time.Time `json:"lockedUntil,omitempty"`
}

type Store struct{ client *es.Client }

func NewStore(client *es.Client) *Store { return &Store{client: client} }

func ValidatePassword(password string) error {
	if n := utf8.RuneCountInString(password); n < 12 || n > 128 {
		return ErrInvalidPassword
	}
	return nil
}

func (s *Store) SetPassword(ctx context.Context, username, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	hit, err := s.client.Get(ctx, resourceName, username)
	if err != nil {
		if es.IsStatus(err, 404) {
			return ErrUserNotFound
		}
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	cred := Credential{PasswordHash: hash, PasswordUpdatedAt: time.Now().UTC()}
	return s.update(ctx, username, hit, cred)
}

func NewPasswordCredential(password string) (json.RawMessage, error) {
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Credential{PasswordHash: hash, PasswordUpdatedAt: time.Now().UTC()})
}

func (s *Store) Authenticate(ctx context.Context, username, password string) (bool, error) {
	if n := utf8.RuneCountInString(password); n < 1 || n > 128 {
		return false, nil
	}
	hit, cred, err := s.get(ctx, username)
	if err != nil {
		if es.IsStatus(err, 404) {
			// Perform comparable work for unknown users to reduce username timing leaks.
			_, _ = HashPassword(password)
			return false, nil
		}
		return false, err
	}
	var user struct {
		Spec struct {
			Enabled *bool `json:"enabled"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(hit.Document.Data, &user); err != nil {
		return false, err
	}
	if user.Spec.Enabled == nil || !*user.Spec.Enabled {
		return false, nil
	}
	now := time.Now().UTC()
	if cred.LockedUntil != nil && now.Before(*cred.LockedUntil) {
		return false, nil
	}
	ok, err := VerifyPassword(password, cred.PasswordHash)
	if err != nil {
		return false, err
	}
	if ok {
		if cred.FailedAttempts != 0 || cred.LockedUntil != nil {
			cred.FailedAttempts = 0
			cred.LockedUntil = nil
			if err := s.update(ctx, username, hit, cred); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	cred.FailedAttempts++
	if cred.FailedAttempts >= maxFailures {
		until := now.Add(lockDuration)
		cred.LockedUntil = &until
	}
	if err := s.update(ctx, username, hit, cred); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) get(ctx context.Context, username string) (*es.Hit, Credential, error) {
	hit, err := s.client.Get(ctx, resourceName, username)
	if err != nil {
		return nil, Credential{}, err
	}
	var cred Credential
	if len(hit.Document.Credential) == 0 {
		return nil, Credential{}, &es.HTTPError{StatusCode: 404, Body: "credential not found"}
	}
	if err := json.Unmarshal(hit.Document.Credential, &cred); err != nil {
		return nil, Credential{}, err
	}
	return hit, cred, nil
}

func (s *Store) update(ctx context.Context, username string, hit *es.Hit, cred Credential) error {
	data, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	doc := hit.Document
	doc.Credential = data
	_, err = s.client.Update(ctx, resourceName, username, doc, hit.SeqNo, hit.PrimaryTerm)
	return err
}

func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, keyLength)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memoryKiB, iterations, parallelism, b64.EncodeToString(salt), b64.EncodeToString(hash)), nil
}

func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false, errors.New("invalid password hash")
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return false, errors.New("invalid password hash parameters")
	}
	parse := func(value, prefix string) (uint64, error) {
		if !strings.HasPrefix(value, prefix) {
			return 0, errors.New("invalid password hash parameters")
		}
		return strconv.ParseUint(strings.TrimPrefix(value, prefix), 10, 32)
	}
	m, err := parse(params[0], "m=")
	if err != nil {
		return false, err
	}
	t, err := parse(params[1], "t=")
	if err != nil {
		return false, err
	}
	p, err := parse(params[2], "p=")
	if err != nil {
		return false, err
	}
	if m == 0 || t == 0 || p == 0 || m > 1<<20 || t > 100 || p > 255 {
		return false, errors.New("unsafe password hash parameters")
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("invalid password hash")
	}
	got := argon2.IDKey([]byte(password), salt, uint32(t), uint32(m), uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
