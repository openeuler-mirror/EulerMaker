package mutation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
)

type Object map[string]any

func ParseObject(data []byte) (Object, error) { return decodeObject(data) }

type Error struct {
	Status int
	Cause  error
}

func (e *Error) Error() string { return e.Cause.Error() }

func reject(status int, message string) error {
	return &Error{Status: status, Cause: errors.New(message)}
}

// Prepare constructs the exact complete object that will be sent in place of
// an external PUT or PATCH. Callers must authorize old and candidate before
// forwarding; PATCH is always converted to PUT.
func Prepare(old, incoming []byte, method, contentType string, maxBytes int64) ([]byte, Object, Object, error) {
	if int64(len(incoming)) > maxBytes {
		return nil, nil, nil, reject(http.StatusRequestEntityTooLarge, "request body too large")
	}
	oldObject, err := decodeObject(old)
	if err != nil {
		return nil, nil, nil, reject(http.StatusBadGateway, "invalid upstream object")
	}
	mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	var candidate []byte
	switch method {
	case http.MethodPut:
		if mediaType != "application/json" {
			return nil, nil, nil, reject(http.StatusUnsupportedMediaType, "PUT requires application/json")
		}
		candidate = incoming
	case http.MethodPatch:
		switch mediaType {
		case "application/merge-patch+json":
			if err := validateJSON(incoming); err != nil {
				return nil, nil, nil, reject(http.StatusBadRequest, "invalid merge patch")
			}
			candidate, err = jsonpatch.MergePatch(old, incoming)
		case "application/json-patch+json":
			if err := validateJSON(incoming); err != nil {
				return nil, nil, nil, reject(http.StatusBadRequest, "invalid JSON patch")
			}
			patch, parseErr := jsonpatch.DecodePatch(incoming)
			if parseErr != nil {
				return nil, nil, nil, reject(http.StatusBadRequest, "invalid JSON patch")
			}
			candidate, err = patch.Apply(old)
			if errors.Is(err, jsonpatch.ErrTestFailed) {
				return nil, nil, nil, reject(http.StatusConflict, "JSON patch test failed")
			}
		default:
			return nil, nil, nil, reject(http.StatusUnsupportedMediaType, "unsupported PATCH media type")
		}
		if err != nil {
			return nil, nil, nil, reject(http.StatusBadRequest, "patch cannot be applied")
		}
	default:
		return nil, nil, nil, reject(http.StatusMethodNotAllowed, "only PUT and PATCH can prepare updates")
	}
	if int64(len(candidate)) > maxBytes {
		return nil, nil, nil, reject(http.StatusRequestEntityTooLarge, "candidate object too large")
	}
	object, err := decodeObject(candidate)
	if err != nil {
		return nil, nil, nil, reject(http.StatusBadRequest, "candidate must be a single JSON object")
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, nil, nil, reject(http.StatusBadRequest, "candidate cannot be encoded")
	}
	return canonical, oldObject, object, nil
}

func decodeObject(data []byte) (Object, error) {
	if err := validateJSON(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object Object
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return object, nil
}

// validateJSON rejects duplicate keys at every depth, malformed values and
// trailing content before encoding/json would silently keep the last key.
func validateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON content")
	}
	return nil
}

func scanValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return nil
}
