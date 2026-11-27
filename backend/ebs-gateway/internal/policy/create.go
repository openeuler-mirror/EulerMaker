package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
)

func (a *Authorizer) PrepareCreate(ctx context.Context, who identity.Principal, route Route, raw []byte) ([]byte, error) {
	object, err := mutation.ParseObject(raw)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: "invalid JSON object"}
	}
	switch route.Resource {
	case "projects":
		if err := a.prepareProjectCreate(ctx, who, object); err != nil {
			return nil, err
		}
	case "runners":
		if who.Type != identity.RunnerType {
			break
		}
		if err := validateRunnerCreate(object, who.Subject); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode object: %w", err)
	}
	return encoded, nil
}
