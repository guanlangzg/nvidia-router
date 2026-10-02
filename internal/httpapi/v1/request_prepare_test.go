package v1

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/modelcatalog"
	"nvidia-router/internal/observability"
)

type prepareRequestFake struct {
	modelID       string
	stream        bool
	requirements  modelcatalog.Requirements
	requested     string
	reasoning     bool
	body          []byte
	marshalErr    error
	marshalCalled bool
	gotModel      modelcatalog.Model
}

func (f *prepareRequestFake) PublicModelID() string { return f.modelID }

func (f *prepareRequestFake) Stream() bool { return f.stream }

func (f *prepareRequestFake) Requirements() modelcatalog.Requirements { return f.requirements }

func (f *prepareRequestFake) RequestedReasoningLevel() string { return f.requested }

func (f *prepareRequestFake) ReasoningRequested() bool { return f.reasoning }

func (f *prepareRequestFake) MarshalForWithOptions(model modelcatalog.Model) ([]byte, error) {
	f.marshalCalled = true
	f.gotModel = model
	if f.marshalErr != nil {
		return nil, f.marshalErr
	}
	return append([]byte(nil), f.body...), nil
}

type prepareResolverFunc func(context.Context, string, modelcatalog.Requirements) (modelcatalog.Model, error)

func (f prepareResolverFunc) Resolve(ctx context.Context, id string, requirements modelcatalog.Requirements) (modelcatalog.Model, error) {
	return f(ctx, id, requirements)
}

func prepareContext() (context.Context, *observability.RequestState) {
	return observability.WithRequestState(context.Background())
}

func TestPrepareModelRequestSuccessPreservesResolveAndReasoningMetadata(t *testing.T) {
	ctx, state := prepareContext()
	requirements := modelcatalog.Requirements{Kind: modelcatalog.KindChat, Vision: true, Tools: true, Reasoning: true}
	request := &prepareRequestFake{
		modelID: "public/model", stream: true, requirements: requirements,
		requested: "high", reasoning: true,
		body: []byte(`{"model":"upstream/model","reasoning_effort":"high"}`),
	}
	model := modelcatalog.Model{ID: 42, PublicID: request.modelID, UpstreamID: "upstream/model", Kind: modelcatalog.KindChat, Enabled: true, SupportsReasoning: true}
	var gotRequirements modelcatalog.Requirements
	prepared, err := prepareModelRequest(ctx, request, prepareResolverFunc(func(_ context.Context, id string, got modelcatalog.Requirements) (modelcatalog.Model, error) {
		if id != request.modelID {
			t.Fatalf("resolver model id = %q, want %q", id, request.modelID)
		}
		gotRequirements = got
		return model, nil
	}))
	if err != nil {
		t.Fatalf("prepareModelRequest: %v", err)
	}
	if !reflect.DeepEqual(gotRequirements, requirements) {
		t.Fatalf("requirements = %#v, want %#v", gotRequirements, requirements)
	}
	if !reflect.DeepEqual(prepared.Model, model) || prepared.Stream != request.stream || !reflect.DeepEqual(prepared.Body, request.body) {
		t.Fatalf("prepared result = %#v, want model/body/stream preserved", prepared)
	}
	if !request.marshalCalled {
		t.Fatal("marshal call = not called, want called")
	}
	snapshot := state.Snapshot()
	if snapshot.ModelID != request.modelID || !snapshot.IsStream {
		t.Fatalf("model observation = %q/%v, want %q/true", snapshot.ModelID, snapshot.IsStream, request.modelID)
	}
	if snapshot.ReasoningRequestedLevel != "high" || snapshot.ReasoningEffectiveLevel != "high" || !snapshot.ReasoningRequested || snapshot.ReasoningWireFields != "reasoning_effort" || snapshot.ReasoningSource != "client" {
		t.Fatalf("reasoning observation = requested:%v wire:%q source:%q levels:%q/%q", snapshot.ReasoningRequested, snapshot.ReasoningWireFields, snapshot.ReasoningSource, snapshot.ReasoningRequestedLevel, snapshot.ReasoningEffectiveLevel)
	}
}

func TestPrepareModelRequestAttributesInvalidReasoningToClientFromFinalBody(t *testing.T) {
	ctx, state := prepareContext()
	request := &prepareRequestFake{
		modelID: "public/model",
		body:    []byte(`{"reasoning_effort":"bogus","reasoning":{"effort":"high"}}`),
	}
	model := modelcatalog.Model{ID: 42, PublicID: request.modelID, UpstreamID: "upstream/model", Kind: modelcatalog.KindChat, Enabled: true}
	prepared, err := prepareModelRequest(ctx, request, prepareResolverFunc(func(context.Context, string, modelcatalog.Requirements) (modelcatalog.Model, error) {
		return model, nil
	}))
	if err != nil {
		t.Fatalf("prepareModelRequest: %v", err)
	}
	if prepared.ReasoningRequested != true || prepared.ReasoningWireFields != "reasoning_effort,reasoning" || prepared.RequestedReasoningLevel != "" || prepared.EffectiveReasoningLevel != "" {
		t.Fatalf("prepared reasoning metadata = requested:%v fields:%q levels:%q/%q", prepared.ReasoningRequested, prepared.ReasoningWireFields, prepared.RequestedReasoningLevel, prepared.EffectiveReasoningLevel)
	}
	snapshot := state.Snapshot()
	if !snapshot.ReasoningRequested || snapshot.ReasoningWireFields != "reasoning_effort,reasoning" || snapshot.ReasoningSource != "client" || snapshot.ReasoningRequestedLevel != "" || snapshot.ReasoningEffectiveLevel != "" {
		t.Fatalf("reasoning observation = requested:%v fields:%q source:%q levels:%q/%q", snapshot.ReasoningRequested, snapshot.ReasoningWireFields, snapshot.ReasoningSource, snapshot.ReasoningRequestedLevel, snapshot.ReasoningEffectiveLevel)
	}
}

func TestPrepareModelRequestResolveFailureRecordsCapabilityCodeAndSkipsMarshal(t *testing.T) {
	ctx, state := prepareContext()
	request := &prepareRequestFake{modelID: "public/model", stream: true, requested: "low", reasoning: true, body: []byte(`{}`)}
	resolveErr := modelcatalog.ErrCapabilityUnverified
	_, err := prepareModelRequest(ctx, request, prepareResolverFunc(func(context.Context, string, modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{}, resolveErr
	}))
	var publicErr *apierror.Error
	if err == nil || !errors.As(err, &publicErr) || publicErr.Code != "capability_unverified" {
		t.Fatalf("error = %v, want mapped capability error", err)
	}
	if request.marshalCalled {
		t.Fatal("MarshalForWithOptions called after Resolve failure")
	}
	snapshot := state.Snapshot()
	if snapshot.ErrorCode == nil || *snapshot.ErrorCode != "capability_unverified" {
		t.Fatalf("error code = %#v, want capability_unverified", snapshot.ErrorCode)
	}
	if snapshot.ModelID != request.modelID || !snapshot.IsStream || snapshot.ReasoningRequestedLevel != "low" {
		t.Fatalf("observation = model:%q stream:%v requested_level:%q", snapshot.ModelID, snapshot.IsStream, snapshot.ReasoningRequestedLevel)
	}
}

func TestPrepareModelRequestMarshalFailureSkipsProviderAndEffectiveReasoning(t *testing.T) {
	ctx, state := prepareContext()
	marshalErr := errors.New("marshal failed")
	request := &prepareRequestFake{modelID: "public/model", requested: "high", reasoning: true, marshalErr: marshalErr}
	model := modelcatalog.Model{ID: 9, PublicID: request.modelID, UpstreamID: "upstream/model", Kind: modelcatalog.KindChat, Enabled: true}
	_, err := prepareModelRequest(ctx, request, prepareResolverFunc(func(context.Context, string, modelcatalog.Requirements) (modelcatalog.Model, error) {
		return model, nil
	}))
	if !errors.Is(err, marshalErr) {
		t.Fatalf("error = %v, want %v", err, marshalErr)
	}
	if !request.marshalCalled {
		t.Fatal("MarshalForWithOptions was not called")
	}
	snapshot := state.Snapshot()
	if snapshot.ReasoningEffectiveLevel != "" || snapshot.ReasoningRequested {
		t.Fatalf("reasoning observation after marshal failure = requested:%v effective:%q", snapshot.ReasoningRequested, snapshot.ReasoningEffectiveLevel)
	}
}
