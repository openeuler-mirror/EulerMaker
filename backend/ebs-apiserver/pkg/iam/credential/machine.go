package credential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	iamv1 "ebs-api/iam/v1"
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

func (s *Store) AuthenticateMachine(ctx context.Context, name, secret string) (iamv1.MachineAccountSpec, bool, error) {
	if len(secret) == 0 || len(secret) > 256 {
		return iamv1.MachineAccountSpec{}, false, nil
	}
	hit, err := s.client.Get(ctx, "machineaccount", name)
	if err != nil {
		if es.IsStatus(err, 404) {
			_, _ = HashPassword(secret)
			return iamv1.MachineAccountSpec{}, false, nil
		}
		return iamv1.MachineAccountSpec{}, false, err
	}
	var account iamv1.MachineAccount
	var doc es.IAMDocument
	if err := hit.Decode(&doc); err != nil {
		return iamv1.MachineAccountSpec{}, false, err
	}
	if err := json.Unmarshal(doc.Data, &account); err != nil {
		return iamv1.MachineAccountSpec{}, false, err
	}
	if account.Spec.Type == "" {
		account.Spec.Type = iamv1.MachineAccountRunner
	}
	if !account.Spec.Type.IsValid() {
		return iamv1.MachineAccountSpec{}, false, errors.New("invalid stored machine account type")
	}
	var cred MachineCredential
	if len(doc.Credential) == 0 || json.Unmarshal(doc.Credential, &cred) != nil {
		return iamv1.MachineAccountSpec{}, false, nil
	}
	now := time.Now().UTC()
	if cred.LockedUntil != nil && now.Before(*cred.LockedUntil) {
		return iamv1.MachineAccountSpec{}, false, nil
	}
	ok, err := VerifyPassword(secret, cred.SecretHash)
	if err != nil {
		return iamv1.MachineAccountSpec{}, false, err
	}
	if ok {
		if cred.FailedAttempts == 0 && cred.LockedUntil == nil {
			return account.Spec, true, nil
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
		return iamv1.MachineAccountSpec{}, false, err
	}
	doc.Credential = data
	if _, err := s.client.Update(ctx, "machineaccount", name, doc, hit.SeqNo, hit.PrimaryTerm); err != nil {
		return iamv1.MachineAccountSpec{}, false, err
	}
	if !ok {
		return iamv1.MachineAccountSpec{}, false, nil
	}
	return account.Spec, true, nil
}
