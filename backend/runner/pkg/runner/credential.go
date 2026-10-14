package runner

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
)

const maxCredentialFileSize = 1 << 20

var dns1123LabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type MachineCredential struct {
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
}

func LoadMachineCredential(path string) (MachineCredential, error) {
	file, err := os.Open(path)
	if err != nil {
		return MachineCredential{}, fmt.Errorf("open machine credential file: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxCredentialFileSize+1))
	if err != nil {
		return MachineCredential{}, fmt.Errorf("read machine credential file: %w", err)
	}
	if len(data) > maxCredentialFileSize {
		return MachineCredential{}, fmt.Errorf("machine credential file exceeds %d bytes", maxCredentialFileSize)
	}
	if err := validateUniqueJSONKeys(data); err != nil {
		return MachineCredential{}, fmt.Errorf("decode machine credential file: %w", err)
	}

	var credential MachineCredential
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil {
		return MachineCredential{}, fmt.Errorf("decode machine credential file: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return MachineCredential{}, fmt.Errorf("decode machine credential file: %w", err)
	}
	if err := validateMachineCredential(credential); err != nil {
		return MachineCredential{}, err
	}
	return credential, nil
}

func validateMachineCredential(credential MachineCredential) error {
	if !validDNS1123Label(credential.ClientID) {
		return fmt.Errorf("machine credential clientID must be a DNS1123 label")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(credential.ClientSecret)
	if err != nil || len(decoded) < 32 || len(credential.ClientSecret) > 256 {
		return fmt.Errorf("machine credential clientSecret must be unpadded base64url with at least 32 bytes of entropy")
	}
	return nil
}

func validDNS1123Label(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dns1123LabelPattern.MatchString(value)
}

func validateUniqueJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := keys[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
