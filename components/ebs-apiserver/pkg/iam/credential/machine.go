package credential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"ebs-apiserver/pkg/storage/es"
)

var ErrInvalidClientSecret = errors.New("client secret must be base64url with at least 32 bytes of entropy")

type MachineCredential struct {
	SecretHash      string     `json:"secretHash"`
	SecretCreatedAt time.Time  `json:"secretCreatedAt"`
	FailedAttempts  int        `json:"failedAttempts"`
	LockedUntil     *time.Time `json:"lockedUntil,omitempty"`
}

func ValidateClientSecret(secret string) error {
	if len(secret) == 0 || len(secret) > 256 {
		return ErrInvalidClientSecret
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) < 32 {
		return ErrInvalidClientSecret
	}
	return nil
}

func NewMachineCredential(secret string) (json.RawMessage, error) {
	if err := ValidateClientSecret(secret); err != nil {
		return nil, err
	}
	hash, err := HashPassword(secret)
	if err != nil {
		return nil, err
	}
	return json.Marshal(MachineCredential{SecretHash: hash, SecretCreatedAt: time.Now().UTC()})
}

func (s *Store) AuthenticateMachine(ctx context.Context, name, secret string) (int64, bool, error) {
	if len(secret) == 0 || len(secret) > 256 {
		return 0, false, nil
	}
	hit, err := s.client.Get(ctx, "machineaccount", name)
	if err != nil {
		if es.IsStatus(err, 404) {
			_, _ = HashPassword(secret)
			return 0, false, nil
		}
		return 0, false, err
	}
	var account struct {
		Spec struct {
			TokenTTLSeconds int64 `json:"tokenTTLSeconds"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(hit.Document.Data, &account); err != nil {
		return 0, false, err
	}
	var cred MachineCredential
	if len(hit.Document.Credential) == 0 || json.Unmarshal(hit.Document.Credential, &cred) != nil {
		return 0, false, nil
	}
	now := time.Now().UTC()
	if cred.LockedUntil != nil && now.Before(*cred.LockedUntil) {
		return 0, false, nil
	}
	ok, err := VerifyPassword(secret, cred.SecretHash)
	if err != nil {
		return 0, false, err
	}
	if ok {
		if cred.FailedAttempts == 0 && cred.LockedUntil == nil {
			return account.Spec.TokenTTLSeconds, true, nil
		}
		cred.FailedAttempts = 0
		cred.LockedUntil = nil
	} else {
		cred.FailedAttempts++
		if cred.FailedAttempts >= maxFailures {
			until := now.Add(lockDuration)
			cred.LockedUntil = &until
		}
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return 0, false, err
	}
	doc := hit.Document
	doc.Credential = data
	if _, err := s.client.Update(ctx, "machineaccount", name, doc, hit.SeqNo, hit.PrimaryTerm); err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	return account.Spec.TokenTTLSeconds, true, nil
}
