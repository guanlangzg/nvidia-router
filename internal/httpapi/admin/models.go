package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nvidia-router/internal/modelcatalog"
	"nvidia-router/internal/xkproxy"
)

type modelManager interface {
	DiscoverCandidates(context.Context, int64) ([]modelcatalog.Candidate, error)
	SaveSelectionResult(context.Context, []modelcatalog.Selection) (modelcatalog.MutationResult, error)
	List(context.Context) ([]modelcatalog.Model, error)
	PatchResult(context.Context, int64, modelcatalog.Patch) (modelcatalog.Model, modelcatalog.Kind, error)
	VerifyAndUnblock(context.Context, int64, int64) (modelcatalog.Model, error)
	DeleteModel(context.Context, int64) error
	TestCandidate(context.Context, string, string) error
}

type modelVerificationDTO struct {
	KeyID int64 `json:"key_id"`
}

type candidateKeySource interface {
	FirstEnabledID(context.Context) (int64, error)
}

type modelStateSync interface {
	SetModelEnabled(int64, bool)
	ClearModelBlocks(int64)
	SetModelBlock(int64, int64, bool)
}

type Models struct {
	service    modelManager
	keys       candidateKeySource
	sync       modelStateSync
	mutationMu sync.Mutex
	// testSlots bounds how many candidate probes may hit the upstreams at once,
	// so one unruly admin client cannot fan out an unbounded probe storm.
	testSlots chan struct{}
}

// candidateTestMaxConcurrency matches the busiest mode the test-job runner
// offers, keeping candidate sweeps no heavier than whitelist batch tests.
const candidateTestMaxConcurrency = 4

type candidateDTO struct {
	PublicID                string            `json:"public_id"`
	UpstreamID              string            `json:"upstream_id"`
	DisplayName             string            `json:"display_name"`
	Kind                    modelcatalog.Kind `json:"kind"`
	Provider                string            `json:"provider"`
	Channel                 string            `json:"channel"`
	Badge                   string            `json:"badge"`
	Status                  string            `json:"status"`
	Capabilities            []string          `json:"capabilities"`
	SupportsVision          bool              `json:"supports_vision"`
	SupportsTools           bool              `json:"supports_tools"`
	ToolsStatus             string            `json:"tools_status"`
	SupportsReasoning       bool              `json:"supports_reasoning"`
	ReasoningStatus         string            `json:"reasoning_status"`
	ReasoningWireFormat     string            `json:"reasoning_wire_format"`
	ReasoningLevels         []string          `json:"reasoning_levels,omitempty"`
	ReasoningMinBudget      int               `json:"reasoning_min_budget,omitempty"`
	ReasoningMaxBudget      int               `json:"reasoning_max_budget,omitempty"`
	ReasoningZeroAllowed    bool              `json:"reasoning_zero_allowed,omitempty"`
	ReasoningDynamicAllowed bool              `json:"reasoning_dynamic_allowed,omitempty"`
}
type modelDTO struct {
	ID                      int64             `json:"id"`
	PublicID                string            `json:"public_id"`
	UpstreamID              string            `json:"upstream_id"`
	DisplayName             string            `json:"display_name"`
	Kind                    modelcatalog.Kind `json:"kind"`
	Provider                string            `json:"provider"`
	Enabled                 bool              `json:"enabled"`
	SupportsVision          bool              `json:"supports_vision"`
	SupportsTools           bool              `json:"supports_tools"`
	ToolsStatus             string            `json:"tools_status"`
	ToolsVerifiedAt         *time.Time        `json:"tools_verified_at,omitempty"`
	SupportsReasoning       bool              `json:"supports_reasoning"`
	ReasoningStatus         string            `json:"reasoning_status"`
	ReasoningWireFormat     string            `json:"reasoning_wire_format"`
	ReasoningLevels         []string          `json:"reasoning_levels,omitempty"`
	ReasoningMinBudget      int               `json:"reasoning_min_budget,omitempty"`
	ReasoningMaxBudget      int               `json:"reasoning_max_budget,omitempty"`
	ReasoningZeroAllowed    bool              `json:"reasoning_zero_allowed,omitempty"`
	ReasoningDynamicAllowed bool              `json:"reasoning_dynamic_allowed,omitempty"`
	CapabilityVerifiedAt    *time.Time        `json:"capability_verified_at,omitempty"`
	// StreamFirstTokenTimeoutMS / StreamIdleTimeoutMS are per-model overrides of
	// the global streaming windows; nil means "use the global setting".
	StreamFirstTokenTimeoutMS *int `json:"stream_first_token_timeout_ms,omitempty"`
	StreamIdleTimeoutMS       *int `json:"stream_idle_timeout_ms,omitempty"`
	// ContextLength is the operator-declared context window (tokens); 0 means
	// undeclared and is surfaced as-is so the admin UI can show it explicitly.
	ContextLength   int     `json:"context_length"`
	BlockedByKeyIDs []int64 `json:"blocked_by_key_ids"`
}
type selectionDTO struct {
	PublicID                string            `json:"public_id"`
	UpstreamID              string            `json:"upstream_id"`
	DisplayName             string            `json:"display_name"`
	Kind                    modelcatalog.Kind `json:"kind"`
	Provider                string            `json:"provider,omitempty"`
	Enabled                 bool              `json:"enabled"`
	SupportsVision          bool              `json:"supports_vision"`
	SupportsTools           bool              `json:"supports_tools"`
	SupportsReasoning       bool              `json:"supports_reasoning"`
	ReasoningStatus         string            `json:"reasoning_status"`
	ReasoningWireFormat     string            `json:"reasoning_wire_format"`
	ReasoningLevels         []string          `json:"reasoning_levels,omitempty"`
	ReasoningMinBudget      int               `json:"reasoning_min_budget,omitempty"`
	ReasoningMaxBudget      int               `json:"reasoning_max_budget,omitempty"`
	ReasoningZeroAllowed    bool              `json:"reasoning_zero_allowed,omitempty"`
	ReasoningDynamicAllowed bool              `json:"reasoning_dynamic_allowed,omitempty"`
}

func NewModels(service modelManager, keys candidateKeySource, syncer modelStateSync) *Models {
	return &Models{service: service, keys: keys, sync: syncer, testSlots: make(chan struct{}, candidateTestMaxConcurrency)}
}

func (h *Models) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/admin/api/models/candidates" && request.Method == http.MethodGet:
		h.candidates(writer, request)
	case request.URL.Path == "/admin/api/models/candidates/test" && request.Method == http.MethodPost:
		h.testCandidate(writer, request)
	case request.URL.Path == "/admin/api/models" && request.Method == http.MethodGet:
		h.list(writer, request)
	case request.URL.Path == "/admin/api/models" && request.Method == http.MethodPost:
		h.save(writer, request)
	case strings.HasPrefix(request.URL.Path, "/admin/api/key-model-blocks/") && request.Method == http.MethodDelete:
		h.unblock(writer, request)
	case strings.HasPrefix(request.URL.Path, "/admin/api/models/") && request.Method == http.MethodPatch:
		h.patch(writer, request)
	case strings.HasPrefix(request.URL.Path, "/admin/api/models/") && request.Method == http.MethodDelete:
		h.delete(writer, request)
	case strings.HasPrefix(request.URL.Path, "/admin/api/models/") && request.Method == http.MethodPost:
		h.verify(writer, request)

	default:
		http.NotFound(writer, request)
	}
}

func (h *Models) candidates(writer http.ResponseWriter, request *http.Request) {
	keyID, err := h.keys.FirstEnabledID(request.Context())
	if err != nil {
		if h.openCodeFreeConfigured() && errors.Is(err, sql.ErrNoRows) {
			keyID = 0
		} else {
			writeAdminError(writer, http.StatusServiceUnavailable, "no_available_keys", "No NVIDIA key is available for model discovery.", err)
			return
		}
	}
	items, err := h.service.DiscoverCandidates(request.Context(), keyID)
	if err != nil {
		var proxyErr *xkproxy.Error
		if errors.As(err, &proxyErr) {
			writeAdminError(writer, http.StatusBadGateway, "upstream_proxy_unavailable", "The upstream proxy is temporarily unavailable.", nil)
			return
		}
		writeAdminError(writer, http.StatusBadGateway, "upstream_error", "The NVIDIA model list could not be loaded.", err)
		return
	}
	data := make([]candidateDTO, 0, len(items))
	for _, item := range items {
		data = append(data, toCandidateDTO(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": data})
}

type candidateTestRequest struct {
	Provider   string `json:"provider"`
	UpstreamID string `json:"upstream_id"`
}

type candidateTestResultDTO struct {
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// testCandidate answers "is this not-yet-saved candidate callable?" with one
// read-only probe. The verdict is synchronous and never persisted: candidates
// have no whitelist row, so there is nowhere to store capability results.
func (h *Models) testCandidate(writer http.ResponseWriter, request *http.Request) {
	var input candidateTestRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		writeInvalidRequest(writer, "The candidate test request is invalid.", err)
		return
	}
	upstreamID, err := validateCandidateTestInput(input)
	if err != nil {
		writeInvalidRequest(writer, "The candidate test request is invalid.", err)
		return
	}
	select {
	case h.testSlots <- struct{}{}:
		defer func() { <-h.testSlots }()
	case <-request.Context().Done():
		return
	}
	started := time.Now()
	err = h.service.TestCandidate(request.Context(), input.Provider, upstreamID)
	duration := time.Since(started).Milliseconds()
	result := candidateTestResultDTO{DurationMS: duration}
	if err != nil {
		result.Status = "failed"
		result.Error = safeModelTestError(err)
	} else {
		result.Status = "success"
	}
	writeJSON(writer, http.StatusOK, result)
}

func validateCandidateTestInput(input candidateTestRequest) (string, error) {
	switch input.Provider {
	case modelcatalog.ProviderNVIDIA, modelcatalog.ProviderOpenCodeFree:
	default:
		return "", errors.New("provider must be nvidia or opencodefree")
	}
	upstreamID := strings.TrimSpace(input.UpstreamID)
	if upstreamID == "" || len(upstreamID) > 200 {
		return "", errors.New("upstream_id must be 1-200 characters")
	}
	if strings.ContainsFunc(upstreamID, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errors.New("upstream_id must not contain control characters")
	}
	return upstreamID, nil
}

func (h *Models) list(writer http.ResponseWriter, request *http.Request) {
	items, err := h.service.List(request.Context())
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	data := make([]modelDTO, 0, len(items))
	for _, item := range items {
		data = append(data, toModelDTO(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": data})
}
func (h *Models) save(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Models []selectionDTO `json:"models"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		writeInvalidRequest(writer, "The model selection request is invalid.", err)
		return
	}
	selections := make([]modelcatalog.Selection, 0, len(input.Models))
	for _, item := range input.Models {
		selections = append(selections, item.selection())
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	result, err := h.service.SaveSelectionResult(request.Context(), selections)
	if err != nil {
		h.writeModelError(writer, err)
		return
	}
	h.syncSavedModels(result)
	writeJSON(writer, http.StatusOK, map[string]any{"saved": len(selections)})
}

func (h *Models) syncSavedModels(result modelcatalog.MutationResult) {
	for _, model := range result.Models {
		h.sync.SetModelEnabled(model.ID, model.Enabled)
		if previous, exists := result.PreviousKinds[model.ID]; exists && previous != model.Kind {
			h.sync.ClearModelBlocks(model.ID)
		}
	}
}
func (h *Models) verify(writer http.ResponseWriter, request *http.Request) {
	id, action, ok := parseIDRoute(request.URL.Path, "/admin/api/models/")
	if !ok || (action != "verify" && action != "test") {
		http.NotFound(writer, request)
		return
	}
	var input modelVerificationDTO
	if err := decodeJSON(writer, request, &input); err != nil || input.KeyID <= 0 {
		if err == nil {
			err = errors.New("key_id must be a positive integer")
		}
		writeInvalidRequest(writer, "The model verification request is invalid.", err)
		return
	}
	model, err := h.service.VerifyAndUnblock(request.Context(), input.KeyID, id)
	if err != nil {
		h.writeModelError(writer, err)
		return
	}
	h.sync.SetModelBlock(input.KeyID, id, false)
	writeJSON(writer, http.StatusOK, toModelDTO(model))
}

func (h *Models) patch(writer http.ResponseWriter, request *http.Request) {
	id, action, ok := parseIDRoute(request.URL.Path, "/admin/api/models/")
	if !ok || action != "" {
		http.NotFound(writer, request)
		return
	}
	var input modelcatalog.Patch
	if err := decodeJSON(writer, request, &input); err != nil {
		writeInvalidRequest(writer, "The model update request is invalid.", err)
		return
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	model, previousKind, err := h.service.PatchResult(request.Context(), id, input)
	if err != nil {
		h.writeModelError(writer, err)
		return
	}
	h.sync.SetModelEnabled(model.ID, model.Enabled)
	if previousKind != "" && previousKind != model.Kind {
		h.sync.ClearModelBlocks(model.ID)
	}
	writeJSON(writer, http.StatusOK, toModelDTO(model))
}
func (h *Models) delete(writer http.ResponseWriter, request *http.Request) {
	id, action, ok := parseIDRoute(request.URL.Path, "/admin/api/models/")
	if !ok || action != "" {
		http.NotFound(writer, request)
		return
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	if err := h.service.DeleteModel(request.Context(), id); err != nil {
		h.writeModelError(writer, err)
		return
	}
	h.sync.SetModelEnabled(id, false)
	h.sync.ClearModelBlocks(id)
	writer.WriteHeader(http.StatusNoContent)
}
func (h *Models) unblock(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/admin/api/key-model-blocks/"), "/")
	if len(parts) != 2 {
		http.NotFound(writer, request)
		return
	}
	keyID, e1 := strconv.ParseInt(parts[0], 10, 64)
	modelID, e2 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || keyID <= 0 || modelID <= 0 {
		http.NotFound(writer, request)
		return
	}
	model, err := h.service.VerifyAndUnblock(request.Context(), keyID, modelID)
	if err != nil {
		h.writeModelError(writer, err)
		return
	}
	h.sync.SetModelBlock(keyID, modelID, false)
	writeJSON(writer, http.StatusOK, toModelDTO(model))
}
func (h *Models) writeModelError(writer http.ResponseWriter, err error) {
	var proxyErr *xkproxy.Error
	if errors.As(err, &proxyErr) {
		writeAdminError(writer, http.StatusBadGateway, "upstream_proxy_unavailable", "The upstream proxy is temporarily unavailable.", nil)
		return
	}
	switch {
	case errors.Is(err, modelcatalog.ErrNVIDIAKeyRequired):
		writeAdminError(writer, http.StatusBadRequest, "nvidia_key_required", "A NVIDIA Key is required for this operation.", err)
	case errors.Is(err, modelcatalog.ErrProviderNotConfigured):
		writeAdminError(writer, http.StatusServiceUnavailable, "provider_not_configured", "The selected model provider is not configured.", err)
	case errors.Is(err, modelcatalog.ErrProviderNotRoutable):
		writeAdminError(writer, http.StatusBadRequest, "provider_not_routable", "The selected model provider is not available for production calls.", err)
	case errors.Is(err, modelcatalog.ErrProviderMismatch):
		writeAdminError(writer, http.StatusBadRequest, "provider_mismatch", "The model does not belong to the selected provider.", err)
	case errors.Is(err, modelcatalog.ErrInvalidModelSelection):
		writeAdminError(writer, http.StatusBadRequest, "invalid_request", "The model selection is invalid.", err)
	case errors.Is(err, modelcatalog.ErrCapabilityUnverified):
		writeAdminError(writer, http.StatusBadRequest, "capability_unverified", "The model capability must be verified before it can be enabled.", err)
	case errors.Is(err, modelcatalog.ErrManualTestRequired):
		writeAdminError(writer, http.StatusBadRequest, "manual_test_required", "A successful manual model test is required.", err)
	case errors.Is(err, modelcatalog.ErrModelNotFound):
		writeAdminError(writer, http.StatusNotFound, "model_not_found", "The model was not found.", err)
	case errors.Is(err, modelcatalog.ErrModelVersionConflict):
		writeAdminError(writer, http.StatusConflict, "model_version_conflict", "The model changed while it was being verified.", err)
	default:
		writeInternalError(writer, err)
	}
}
func toCandidateDTO(v modelcatalog.Candidate) candidateDTO {
	provider := v.Provider
	if provider == "" {
		provider = modelcatalog.ProviderNVIDIA
	}
	channel := v.Channel
	if channel == "" {
		channel = provider
	}
	badge := v.Badge
	if badge == "" {
		badge = "NVIDIA"
		if provider == modelcatalog.ProviderOpenCodeFree {
			badge = "OpenCodeFree"
		}
	}
	status := v.Status
	if status == "" {
		status = "available"
	}
	capabilities := append([]string(nil), v.Capabilities...)
	if len(capabilities) == 0 {
		capabilities = append(capabilities, v.CapabilityTags...)
	}
	if len(capabilities) == 0 && v.Kind != "" {
		capabilities = []string{string(v.Kind)}
	}
	publicID := v.PublicID
	if publicID == "" {
		publicID = v.UpstreamID
	}
	toolsStatus := v.ToolsStatus
	if toolsStatus == "" {
		toolsStatus = modelcatalog.ToolsStatusUnknown
	}
	return candidateDTO{
		PublicID: publicID, UpstreamID: v.UpstreamID, DisplayName: v.DisplayName, Kind: v.Kind,
		Provider: provider, Channel: channel, Badge: badge, Status: status, Capabilities: capabilities,
		SupportsVision: v.SupportsVision, SupportsTools: v.SupportsTools, ToolsStatus: toolsStatus, SupportsReasoning: v.SupportsReasoning,
		ReasoningStatus:     v.ReasoningStatus,
		ReasoningWireFormat: v.ReasoningWireFormat,
		ReasoningLevels:     v.ReasoningLevels, ReasoningMinBudget: v.ReasoningMinBudget,
		ReasoningMaxBudget: v.ReasoningMaxBudget, ReasoningZeroAllowed: v.ReasoningZeroAllowed,
		ReasoningDynamicAllowed: v.ReasoningDynamicAllowed,
	}
}
func toModelDTO(v modelcatalog.Model) modelDTO {
	provider := v.Provider
	if provider == "" {
		provider = "nvidia"
	}
	toolsStatus := v.ToolsStatus
	if toolsStatus == "" {
		toolsStatus = modelcatalog.ToolsStatusUnknown
	}
	return modelDTO{
		ID:                        v.ID,
		PublicID:                  v.PublicID,
		UpstreamID:                v.UpstreamID,
		DisplayName:               v.DisplayName,
		Kind:                      v.Kind,
		Provider:                  provider,
		Enabled:                   v.Enabled,
		SupportsVision:            v.SupportsVision,
		SupportsTools:             v.SupportsTools,
		ToolsStatus:               toolsStatus,
		ToolsVerifiedAt:           v.ToolsVerifiedAt,
		SupportsReasoning:         v.SupportsReasoning,
		ReasoningStatus:           v.ReasoningStatus,
		ReasoningWireFormat:       v.ReasoningWireFormat,
		ReasoningLevels:           v.ReasoningLevels,
		ReasoningMinBudget:        v.ReasoningMinBudget,
		ReasoningMaxBudget:        v.ReasoningMaxBudget,
		ReasoningZeroAllowed:      v.ReasoningZeroAllowed,
		ReasoningDynamicAllowed:   v.ReasoningDynamicAllowed,
		CapabilityVerifiedAt:      v.CapabilityVerifiedAt,
		StreamFirstTokenTimeoutMS: v.StreamFirstTokenTimeoutMS,
		StreamIdleTimeoutMS:       v.StreamIdleTimeoutMS,
		ContextLength:             v.ContextLength,
		BlockedByKeyIDs:           v.BlockedByKeyIDs,
	}
}
func (v selectionDTO) selection() modelcatalog.Selection {
	return modelcatalog.Selection{PublicID: v.PublicID, UpstreamID: v.UpstreamID, DisplayName: v.DisplayName, Kind: v.Kind, Provider: v.Provider, Enabled: v.Enabled, SupportsVision: v.SupportsVision, SupportsTools: v.SupportsTools, SupportsReasoning: v.SupportsReasoning, ReasoningStatus: v.ReasoningStatus, ReasoningWireFormat: v.ReasoningWireFormat, ReasoningLevels: v.ReasoningLevels, ReasoningMinBudget: v.ReasoningMinBudget, ReasoningMaxBudget: v.ReasoningMaxBudget, ReasoningZeroAllowed: v.ReasoningZeroAllowed, ReasoningDynamicAllowed: v.ReasoningDynamicAllowed}
}

func (h *Models) openCodeFreeConfigured() bool {
	configured, ok := h.service.(interface{ OpenCodeFreeConfigured() bool })
	return ok && configured.OpenCodeFreeConfigured()
}
