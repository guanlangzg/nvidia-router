package chat

import (
	"encoding/json"
	"errors"
	"fmt"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/compat"
	"nvidia-router/internal/modelcatalog"
)

func rejectUnsupported(fields map[string]json.RawMessage) error {
	raw, ok := fields["store"]
	if !ok {
		return nil
	}
	var store *bool
	if json.Unmarshal(raw, &store) != nil || store == nil {
		return invalidRequest("invalid_parameter", "store", "The store parameter must be a boolean.")
	}
	if *store {
		return invalidRequest("unsupported_parameter", "store", "Stored responses are not supported.")
	}
	return nil
}

func validateModel(request Request, model modelcatalog.Model) error {
	if model.PublicID != request.publicModel || model.UpstreamID == "" || model.Kind != modelcatalog.KindChat || !model.Enabled {
		return fmt.Errorf("prepare chat request: resolved model does not match request")
	}

	return nil
}

func invalidRequest(code, param, message string) *apierror.Error {
	var parameter *string
	if param != "" {
		parameter = &param
	}
	return &apierror.Error{
		Status: 400, Type: "invalid_request_error", Code: code, Message: message, Param: parameter,
	}
}

func compatRequestError(err error) error {
	var validation *compat.ValidationError
	if errors.As(err, &validation) {
		return invalidRequest(validation.Code, validation.Param, validation.Message)
	}
	return invalidRequest("invalid_parameter", "", err.Error())
}
