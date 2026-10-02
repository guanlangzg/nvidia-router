package v1

import (
	"context"

	"nvidia-router/internal/modelcatalog"
	"nvidia-router/internal/observability"
)

// modelRequest is the smallest common surface exposed by the validated Chat
// and Responses requests. Protocol parsing remains in each protocol package;
// this interface only covers the model-resolution and upstream-body phase.
type modelRequest interface {
	PublicModelID() string
	Stream() bool
	Requirements() modelcatalog.Requirements
	RequestedReasoningLevel() string
	ReasoningRequested() bool
	MarshalForWithOptions(modelcatalog.Model) ([]byte, error)
}

// preparedModelRequest contains the values shared by the Chat and Responses
// provider branches. The protocol marshaller owns Body for the request
// lifetime; adapters treat it as read-only so large payloads are not copied on
// the hot path.
type preparedModelRequest struct {
	Model                   modelcatalog.Model
	Body                    []byte
	Stream                  bool
	RequestedReasoningLevel string
	EffectiveReasoningLevel string
	ReasoningRequested      bool
	ReasoningWireFields     string
}

// prepareModelRequest performs the common model/stream observation, model
// resolution, capability error recording, upstream marshalling and reasoning
// metadata observation. It deliberately has no provider or response-writing
// responsibilities: callers can only enter a provider branch after this
// function succeeds.
func prepareModelRequest(ctx context.Context, request modelRequest, resolver ModelResolver) (preparedModelRequest, error) {
	modelID := request.PublicModelID()
	stream := request.Stream()
	observability.SetModel(ctx, modelID, stream)

	requestedReasoningLevel := request.RequestedReasoningLevel()
	observability.SetReasoningLevels(ctx, requestedReasoningLevel, "")

	requirements := request.Requirements()
	observability.SetRequestedCapabilities(ctx, requirements.Vision, requirements.Tools, requirements.Reasoning)
	model, err := resolver.Resolve(ctx, modelID, requirements)
	if err != nil {
		// Keep capability failures distinguishable in request metadata before
		// mapping them to the existing public API error shape.
		recordCapabilityErrorCode(ctx, err)
		return preparedModelRequest{}, modelError(err)
	}

	body, err := request.MarshalForWithOptions(model)
	if err != nil {
		return preparedModelRequest{}, err
	}

	// Marshal succeeded, so it is now safe to publish effective wire metadata.
	// Reasoning is pass-through, so the effective level is whatever the client
	// asked for and the source is always the client when a request carried one.
	effectiveReasoningLevel, reasoningRequested, wireFields := observability.ReasoningMetadataFromBody(body)
	observability.SetReasoningLevels(ctx, requestedReasoningLevel, effectiveReasoningLevel)
	observability.SetReasoningRequest(ctx, reasoningRequested, wireFields)
	if reasoningRequested {
		observability.SetReasoningSource(ctx, "client")
	}

	return preparedModelRequest{
		Model:                   model,
		Body:                    body,
		Stream:                  stream,
		RequestedReasoningLevel: requestedReasoningLevel,
		EffectiveReasoningLevel: effectiveReasoningLevel,
		ReasoningRequested:      reasoningRequested,
		ReasoningWireFields:     wireFields,
	}, nil
}
