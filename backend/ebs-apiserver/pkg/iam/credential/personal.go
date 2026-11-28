package credential

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	iamv1 "ebs-api/iam/v1"
	"ebs-apiserver/pkg/storage/es"
)

const (
	personalCredentialLimit = 5
	personalCredentialDays  = 90
	personalWriteAttempts   = 3
	personalTokenPrefix     = "ebs_pat_v1"
)

var (
	ErrPersonalInput    = errors.New("invalid personal credential input")
	ErrPersonalInvalid  = errors.New("invalid personal credential")
	ErrPersonalLimit    = errors.New("personal credential limit reached")
	ErrPersonalConflict = errors.New("personal credential conflict")
	ErrUserUnavailable  = errors.New("user unavailable")
)

type PersonalCredential struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type personalCredentialRecord struct {
	PersonalCredential
	SecretHash string `json:"secretHash"`
}

type CreatedPersonalCredential struct {
	PersonalCredential
	Token string `json:"token"`
}

type PersonalAuthentication struct {
	Authenticated bool      `json:"authenticated"`
	Username      string    `json:"username"`
	UID           types.UID `json:"uid"`
	Scopes        []string  `json:"scopes"`
	CredentialID  string    `json:"credentialID"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

func validPersonalID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

func personalTokenParts(token string) (string, string, error) {
	if len(token) > 512 {
		return "", "", ErrPersonalInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != personalTokenPrefix || !validPersonalID(parts[2]) {
		return "", "", ErrPersonalInvalid
	}
	username, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(username) != parts[1] || len(utilvalidation.IsDNS1123Label(string(username))) != 0 {
		return "", "", ErrPersonalInvalid
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != parts[3] {
		return "", "", ErrPersonalInvalid
	}
	return string(username), parts[2], nil
}

func (s *Store) CreatePersonal(ctx context.Context, username, name string, expiresInDays *int) (*CreatedPersonalCredential, error) {
	name = strings.TrimSpace(name)
	days := personalCredentialDays
	if expiresInDays != nil {
		days = *expiresInDays
	}
	if len(utilvalidation.IsDNS1123Label(username)) != 0 || !utf8.ValidString(name) || name == "" ||
		utf8.RuneCountInString(name) > 64 || strings.IndexFunc(name, unicode.IsControl) >= 0 || days < 1 || days > personalCredentialDays {
		return nil, ErrPersonalInput
	}
	var random [48]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(random[:16])
	token := strings.Join([]string{personalTokenPrefix, base64.RawURLEncoding.EncodeToString([]byte(username)), id, base64.RawURLEncoding.EncodeToString(random[16:])}, ".")
	digest := sha256.Sum256([]byte(token))
	var result PersonalCredential
	err := s.updatePersonal(ctx, username, func(cred *Credential, now time.Time) (bool, error) {
		active := make([]personalCredentialRecord, 0, personalCredentialLimit)
		for _, item := range cred.PersonalCredentials {
			if item.ID == id {
				return false, ErrPersonalConflict
			}
			if now.Before(item.ExpiresAt) {
				active = append(active, item)
			}
		}
		if len(active) >= personalCredentialLimit {
			return false, ErrPersonalLimit
		}
		result = PersonalCredential{
			ID:        id,
			Name:      name,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour),
		}
		cred.PersonalCredentials = append(active, personalCredentialRecord{PersonalCredential: result, SecretHash: hex.EncodeToString(digest[:])})
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return &CreatedPersonalCredential{PersonalCredential: result, Token: token}, nil
}

func (s *Store) ListPersonal(ctx context.Context, username string) ([]PersonalCredential, error) {
	_, cred, _, err := s.getPersonalUser(ctx, username)
	if err != nil {
		return nil, err
	}
	items := make([]PersonalCredential, 0, personalCredentialLimit)
	now := s.now()
	for _, item := range cred.PersonalCredentials {
		if now.Before(item.ExpiresAt) {
			items = append(items, item.PersonalCredential)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	return items, nil
}

func (s *Store) DeletePersonal(ctx context.Context, username, id string) error {
	if !validPersonalID(id) {
		return ErrPersonalInput
	}
	return s.updatePersonal(ctx, username, func(cred *Credential, _ time.Time) (bool, error) {
		items := make([]personalCredentialRecord, 0, len(cred.PersonalCredentials))
		for _, item := range cred.PersonalCredentials {
			if item.ID != id {
				items = append(items, item)
			}
		}
		changed := len(items) != len(cred.PersonalCredentials)
		cred.PersonalCredentials = items
		return changed, nil
	})
}

func (s *Store) AuthenticatePersonal(ctx context.Context, token string) (*PersonalAuthentication, error) {
	username, id, err := personalTokenParts(token)
	if err != nil {
		return nil, err
	}
	_, cred, user, err := s.getPersonalUser(ctx, username)
	if errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrUserUnavailable) {
		return nil, ErrPersonalInvalid
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(token))
	for _, item := range cred.PersonalCredentials {
		if item.ID != id || !s.now().Before(item.ExpiresAt) {
			continue
		}
		stored, err := hex.DecodeString(item.SecretHash)
		if err != nil || len(stored) != sha256.Size {
			return nil, errors.New("invalid stored personal credential digest")
		}
		if subtle.ConstantTimeCompare(stored, digest[:]) != 1 {
			return nil, ErrPersonalInvalid
		}
		return &PersonalAuthentication{
			Authenticated: true,
			Username:      user.Name,
			UID:           user.UID,
			Scopes:        user.Spec.Scopes,
			CredentialID:  id,
			ExpiresAt:     item.ExpiresAt,
		}, nil
	}
	return nil, ErrPersonalInvalid
}

func (s *Store) getPersonalUser(ctx context.Context, username string) (*es.Hit, Credential, *iamv1.User, error) {
	if len(utilvalidation.IsDNS1123Label(username)) != 0 {
		return nil, Credential{}, nil, ErrPersonalInput
	}
	hit, err := s.client.Get(ctx, resourceName, username)
	if es.IsDocumentNotFound(err) {
		return nil, Credential{}, nil, ErrUserNotFound
	}
	if err != nil {
		return nil, Credential{}, nil, err
	}
	var doc es.IAMDocument
	if err := hit.Decode(&doc); err != nil {
		return nil, Credential{}, nil, err
	}
	var user iamv1.User
	if err := json.Unmarshal(doc.Data, &user); err != nil {
		return nil, Credential{}, nil, err
	}
	if user.Name != username || user.UID == "" || hit.PrimaryTerm <= 0 || hit.SeqNo < 0 {
		return nil, Credential{}, nil, errors.New("invalid stored user identity or version")
	}
	if !CanAuthenticate(&user) {
		return nil, Credential{}, nil, ErrUserUnavailable
	}
	var cred Credential
	if len(doc.Credential) > 0 {
		if err := json.Unmarshal(doc.Credential, &cred); err != nil {
			return nil, Credential{}, nil, err
		}
	}
	return hit, cred, &user, nil
}

// Only definite CAS conflicts are retried; unknown write results must not issue a token.
func (s *Store) updatePersonal(ctx context.Context, username string, mutate func(*Credential, time.Time) (bool, error)) error {
	var uid types.UID
	for attempt := 0; attempt < personalWriteAttempts; attempt++ {
		hit, cred, user, err := s.getPersonalUser(ctx, username)
		if err != nil {
			return err
		}
		if attempt > 0 && user.UID != uid {
			return ErrPersonalConflict
		}
		uid = user.UID
		changed, err := mutate(&cred, s.now().UTC())
		if err != nil || !changed {
			return err
		}
		if err = s.update(ctx, username, hit, cred); !es.IsStatus(err, 409) {
			return err
		}
	}
	return ErrPersonalConflict
}
