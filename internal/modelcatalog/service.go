package modelcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"time"

	"nvidia-router/internal/clock"
	"nvidia-router/internal/compat"
	"nvidia-router/internal/runtimeconfig"
	"nvidia-router/internal/upstream/nvidia"
	"nvidia-router/internal/xkproxy"
)

type SecretProvider interface {
	WithSecret(context.Context, int64, func([]byte) error) error
	// AvailableIDsShuffled lists the keys that may serve a probe right now, in
	// the order they should be tried.
	AvailableIDsShuffled(context.Context) ([]int64, error)
}

type ModelDiscoverer interface {
	Models(context.Context, string) ([]string, error)
}

type OpenCodeFreeClient interface {
	Models(context.Context) ([]string, error)
	Chat(context.Context, runtimeconfig.Snapshot, []byte, bool) (*http.Response, error)
}

type chatModelTester interface {
	Chat(context.Context, runtimeconfig.Snapshot, string, []byte, bool) (*http.Response, error)
}

type embeddingModelTester interface {
	Embeddings(context.Context, runtimeconfig.Snapshot, string, []byte) (*http.Response, error)
}

type asrModelTester interface {
	AudioTranscriptions(context.Context, runtimeconfig.Snapshot, string, []byte, string) (*http.Response, error)
}

type ttsModelTester interface {
	AudioSpeech(context.Context, runtimeconfig.Snapshot, string, []byte) (*http.Response, error)
}

const (
	modelVerificationTimeout    = 30 * time.Second
	maxModelVerificationTimeout = 5 * time.Minute
	// modelProbeMaxTokens bounds base probes. Modern reasoning/instruction models
	// spend output tokens on chat-template prefixes and initial reasoning tokens
	// before visible content; 128 provides a safe buffer against empty-response false negatives.
	modelProbeMaxTokens = 128
	// ocfProbeMaxTokens bounds the OpenCodeFree base probes. Gateway models spend
	// output tokens on invisible chat-template markers before any visible text
	// (observed: a 16-token window on mimo-v2.5-free returned 200 with empty
	// content, flagging a working model as unusable), and hidden reasoning eats
	// the budget the same way. Probes are operator-triggered, so the larger
	// window costs nothing against a wrong verdict.
	ocfProbeMaxTokens = 256
	// toolsProbeMaxTokens bounds the tool-calling probe specifically. Gateway
	// models frequently emit hidden reasoning (or truncated argument JSON)
	// before the tool_call; a 16-token window turned every such model into a
	// permanent false "unsupported". Probes are operator-triggered, so the
	// larger window amortizes to nothing against a wrong verdict.
	toolsProbeMaxTokens = 256
	// probeAttempts bounds how many NVIDIA keys one probe may try. A model that
	// stays unreachable on three different credentials is a model or upstream
	// problem, and more attempts would only burn quota.
	probeAttempts = 3
)

func modelVerificationTimeoutFor(model Model) time.Duration {
	timeout := modelVerificationTimeout
	if model.StreamFirstTokenTimeoutMS != nil && *model.StreamFirstTokenTimeoutMS > 0 {
		configured := time.Duration(*model.StreamFirstTokenTimeoutMS) * time.Millisecond
		if configured > timeout {
			timeout = configured
		}
	}
	if timeout > maxModelVerificationTimeout {
		return maxModelVerificationTimeout
	}
	return timeout
}

type Service struct {
	repository   *Repository
	secrets      SecretProvider
	discoverer   ModelDiscoverer
	descriptor   nvidia.Descriptor
	clock        clock.Clock
	opencodefree OpenCodeFreeClient
}

func NewService(repository *Repository, secrets SecretProvider, discoverer ModelDiscoverer, descriptor nvidia.Descriptor, source clock.Clock) *Service {
	if source == nil {
		source = clock.RealClock{}
	}
	return &Service{repository: repository, secrets: secrets, discoverer: discoverer, descriptor: descriptor, clock: source}
}

func (s *Service) WithOpenCodeFree(client OpenCodeFreeClient) *Service {
	if isNilOpenCodeFreeClient(client) {
		return s
	}
	s.opencodefree = client
	return s
}

func (s *Service) OpenCodeFreeConfigured() bool {
	return !isNilOpenCodeFreeClient(s.opencodefree)
}

func isNilOpenCodeFreeClient(client OpenCodeFreeClient) bool {
	if client == nil {
		return true
	}
	v := reflect.ValueOf(client)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Ptr, reflect.Slice, reflect.Interface, reflect.UnsafePointer:
		return v.IsNil()
	default:
		return false
	}
}

func (s *Service) DiscoverCandidates(ctx context.Context, keyID int64) ([]Candidate, error) {
	candidates := make([]Candidate, 0)
	if keyID > 0 {
		var modelIDs []string
		if err := s.secrets.WithSecret(ctx, keyID, func(secret []byte) error {
			models, err := s.discoverer.Models(ctx, string(secret))
			if err != nil {
				return fmt.Errorf("discover NVIDIA models: %w", err)
			}
			modelIDs = models
			return nil
		}); err != nil {
			return nil, fmt.Errorf("discover model candidates: %w", err)
		}
		for _, modelID := range modelIDs {
			hint := s.descriptor.CapabilityHint(modelID)
			candidates = append(candidates, candidateFromHint(modelID, hint))
		}
	} else if s.opencodefree == nil {
		return nil, ErrNVIDIAKeyRequired
	}
	if s.opencodefree != nil {
		modelIDs, err := s.opencodefree.Models(ctx)
		if err != nil {
			return nil, fmt.Errorf("discover OpenCodeFree models: %w", err)
		}
		for _, modelID := range modelIDs {
			// Only free-tier gateway models are callable from this deployment, so
			// non-free IDs must never surface as whitelist candidates.
			if !isFreeModelID(modelID) {
				continue
			}
			candidates = append(candidates, candidateFromOpenCodeFree(modelID))
		}
	}
	sortCandidates(candidates)
	return candidates, nil
}

func (s *Service) SaveSelection(ctx context.Context, selections []Selection) error {
	_, err := s.SaveSelectionResult(ctx, selections)
	return err
}

func (s *Service) SaveSelectionResult(ctx context.Context, selections []Selection) (MutationResult, error) {
	normalized := make([]Selection, len(selections))
	for index, selection := range selections {
		value, err := normalizeModelSelection(selection)
		if err != nil {
			return MutationResult{}, fmt.Errorf("validate selected model %q: %w", selection.PublicID, err)
		}
		normalized[index] = value
	}
	result, err := s.repository.SaveSelectionsResult(ctx, normalized, s.clock.Now())
	if err != nil {
		return MutationResult{}, fmt.Errorf("save model selection: %w", err)
	}
	return result, nil
}

func (s *Service) List(ctx context.Context) ([]Model, error) {
	models, err := s.repository.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	return models, nil
}

// ValidateNVIDIAKey checks that the selected encrypted credential is readable
// without making an upstream request or changing key/model state.
func (s *Service) ValidateNVIDIAKey(ctx context.Context, keyID int64) error {
	if keyID <= 0 {
		return ErrNVIDIAKeyRequired
	}
	return s.secrets.WithSecret(ctx, keyID, func([]byte) error { return nil })
}

func (s *Service) Patch(ctx context.Context, id int64, patch Patch) (Model, error) {
	updated, _, err := s.PatchResult(ctx, id, patch)
	return updated, err
}

func (s *Service) PatchResult(ctx context.Context, id int64, patch Patch) (Model, Kind, error) {
	model, previousKind, err := s.repository.Patch(ctx, id, patch, s.clock.Now())
	if err != nil {
		return Model{}, "", fmt.Errorf("save model patch: %w", err)
	}
	return model, previousKind, nil
}

// TestModel performs one read-only endpoint probe. It never changes the
// whitelist, capability verification, key-model blocks, or key health state.
func (s *Service) TestModel(ctx context.Context, provider string, keyID, modelID int64) error {
	model, err := s.repository.Get(ctx, modelID)
	if err != nil {
		return fmt.Errorf("load model for read-only test: %w", err)
	}
	if provider == "" {
		provider = defaultModelProvider
	}
	if model.Provider == "" {
		model.Provider = defaultModelProvider
	}
	if model.Provider != provider {
		return fmt.Errorf("%w: model %q belongs to %s", ErrProviderMismatch, model.PublicID, model.Provider)
	}
	testCtx, cancel := context.WithTimeout(ctx, modelVerificationTimeoutFor(model))
	defer cancel()
	switch provider {
	case ProviderNVIDIA:
		if keyID <= 0 {
			return ErrNVIDIAKeyRequired
		}
		return s.testTargetModel(testCtx, keyID, model)
	case ProviderOpenCodeFree:
		if keyID != 0 {
			return fmt.Errorf("%w: OpenCodeFree does not use an NVIDIA key", ErrInvalidModelSelection)
		}
		return s.testOpenCodeFreeModel(testCtx, model)
	default:
		return fmt.Errorf("%w: %s", ErrProviderNotRoutable, provider)
	}
}

// TestModelAuto probes one model without the caller naming a channel or a
// credential: the provider comes from the model row itself and an NVIDIA probe
// picks its own keys. Like TestModel it is read-only and never touches the
// whitelist, capability verification, key health, blocks or request statistics.
func (s *Service) TestModelAuto(ctx context.Context, modelID int64) error {
	model, err := s.repository.Get(ctx, modelID)
	if err != nil {
		return fmt.Errorf("load model for read-only test: %w", err)
	}
	if model.Provider == "" {
		model.Provider = defaultModelProvider
	}
	return s.probeByProvider(ctx, model)
}

// TestCandidate probes a discovery candidate that has no whitelist row yet, so
// the operator can see whether a model is callable before saving it. It reuses
// the read-only TestModelAuto probe path and persists nothing.
func (s *Service) TestCandidate(ctx context.Context, provider, upstreamID string) error {
	if provider == "" {
		provider = defaultModelProvider
	}
	model := Model{UpstreamID: upstreamID, Provider: provider, Kind: KindChat}
	if provider == ProviderNVIDIA {
		candidateCtx, cancel := context.WithTimeout(ctx, maxModelVerificationTimeout)
		defer cancel()
		firstByteTimeoutMS := int(maxModelVerificationTimeout / time.Millisecond)
		model.StreamFirstTokenTimeoutMS = &firstByteTimeoutMS
		return s.probeByProvider(candidateCtx, model)
	}
	return s.probeByProvider(ctx, model)
}

func (s *Service) probeByProvider(ctx context.Context, model Model) error {
	switch model.Provider {
	case ProviderNVIDIA:
		return s.probeNVIDIAModel(ctx, model)
	case ProviderOpenCodeFree:
		// The gateway client already retries across pooled exits, so a single
		// attempt here is one attempt per healthy IP.
		testCtx, cancel := context.WithTimeout(ctx, modelVerificationTimeoutFor(model))
		defer cancel()
		return s.testOpenCodeFreeModel(testCtx, model)
	default:
		return fmt.Errorf("%w: %s", ErrProviderNotRoutable, model.Provider)
	}
}

// probeNVIDIAModel tries the model on up to probeAttempts distinct keys, taken
// in random order so a batch spreads across the fleet. Only a failure that could
// plausibly clear on another credential is retried.
func (s *Service) probeNVIDIAModel(ctx context.Context, model Model) error {
	keyIDs, err := s.secrets.AvailableIDsShuffled(ctx)
	if err != nil {
		return fmt.Errorf("select NVIDIA key for read-only test: %w", err)
	}
	if len(keyIDs) == 0 {
		return ErrNVIDIAKeyRequired
	}
	if len(keyIDs) > probeAttempts {
		keyIDs = keyIDs[:probeAttempts]
	}
	var lastErr error
	for _, keyID := range keyIDs {
		testCtx, cancel := context.WithTimeout(ctx, modelVerificationTimeoutFor(model))
		err := s.testTargetModel(testCtx, keyID, model)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		if !worthAnotherKey(err) {
			return err
		}
	}
	return lastErr
}

// worthAnotherKey reports whether a failed probe may still succeed on a
// different credential. Only an unreachable upstream or a proxy failure
// qualifies: a model that answered 404, or answered with a malformed payload,
// will answer the same way on every key.
func worthAnotherKey(err error) bool {
	var proxyErr *xkproxy.Error
	if errors.As(err, &proxyErr) {
		return true
	}
	return errors.Is(err, ErrUpstreamUnreachable)
}

func (s *Service) VerifyAndUnblock(ctx context.Context, keyID, modelID int64) (Model, error) {
	model, err := s.repository.Get(ctx, modelID)
	if err != nil {
		return Model{}, fmt.Errorf("load model before manual test: %w", err)
	}
	verifyCtx, cancel := context.WithTimeout(ctx, modelVerificationTimeoutFor(model))
	defer cancel()
	if err := s.testTargetModel(verifyCtx, keyID, model); err != nil {
		return Model{}, fmt.Errorf("manual model test: %w", err)
	}
	verified, err := s.repository.VerifyAndUnblock(ctx, keyID, modelID, model.updatedAt, s.clock.Now())
	if err != nil {
		return Model{}, fmt.Errorf("verify model and clear block: %w", err)
	}
	return verified, nil
}

func (s *Service) testTargetModel(ctx context.Context, keyID int64, model Model) error {
	return s.secrets.WithSecret(ctx, keyID, func(secret []byte) error {
		var response *http.Response
		var err error
		// Keep the transport-level first-byte deadline aligned with the
		// verification context. Otherwise a slow model can be cancelled by the
		// fleet-wide request timeout before its model-specific verification window
		// has a chance to take effect.
		snapshot := runtimeconfig.Snapshot{
			FirstByteTimeoutMS: int(modelVerificationTimeoutFor(model) / time.Millisecond),
		}
		switch model.Kind {
		case KindChat:
			tester, ok := s.discoverer.(chatModelTester)
			if !ok {
				return ErrManualTestRequired
			}
			body, marshalErr := json.Marshal(map[string]any{"model": model.UpstreamID, "messages": []map[string]string{{"role": "user", "content": "ping"}}, "max_tokens": modelProbeMaxTokens})
			if marshalErr != nil {
				return marshalErr
			}
			response, err = tester.Chat(ctx, snapshot, string(secret), body, false)
		case KindEmbedding:
			tester, ok := s.discoverer.(embeddingModelTester)
			if !ok {
				return ErrManualTestRequired
			}
			body, marshalErr := json.Marshal(map[string]any{"model": model.UpstreamID, "input": "ping"})
			if marshalErr != nil {
				return marshalErr
			}
			response, err = tester.Embeddings(ctx, snapshot, string(secret), body)
		case KindASR:
			tester, ok := s.discoverer.(asrModelTester)
			if !ok {
				return ErrManualTestRequired
			}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if err := writer.WriteField("model", model.UpstreamID); err != nil {
				return err
			}
			part, partErr := writer.CreateFormFile("file", "probe.wav")
			if partErr != nil {
				return partErr
			}
			if _, err := part.Write(probeWAV); err != nil {
				return err
			}
			if err = writer.Close(); err != nil {
				return err
			}
			response, err = tester.AudioTranscriptions(ctx, snapshot, string(secret), body.Bytes(), writer.FormDataContentType())
		case KindTTS:
			tester, ok := s.discoverer.(ttsModelTester)
			if !ok {
				return ErrManualTestRequired
			}
			body, marshalErr := json.Marshal(map[string]any{"model": model.UpstreamID, "input": "ping", "voice": "Magpie-Multilingual.EN-US.Aria", "response_format": "wav"})
			if marshalErr != nil {
				return marshalErr
			}
			response, err = tester.AudioSpeech(ctx, snapshot, string(secret), body)
		default:
			return ErrManualTestRequired
		}
		if err != nil {
			var proxyErr *xkproxy.Error
			if errors.As(err, &proxyErr) {
				return err
			}
			return ErrUpstreamUnreachable
		}
		if response == nil || response.Body == nil {
			return ErrUpstreamUnreachable
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return probeStatusError(response.StatusCode)
		}
		switch model.Kind {
		case KindChat:
			_, err = nvidia.ValidateNonstreamChat(response)
		case KindEmbedding:
			_, err = nvidia.ValidateNonstreamEmbeddings(response)
		case KindASR:
			_, err = nvidia.ValidateNonstreamAudio(response, false)
		case KindTTS:
			if !isAudioContentType(response.Header.Get("Content-Type")) {
				return ErrManualTestRequired
			}
			err = nvidia.PrimeAudioSpeech(ctx, response)
		}
		if err != nil {
			return probeValidationError(err)
		}
		markProbeComplete(response)
		return nil
	})
}

// markProbeComplete tells the upstream body that a validated non-stream answer
// really did complete. Without it the pooled exit that served a perfectly good
// 200 is charged a request failure on Close, because non-stream bodies defer the
// EOF verdict until a validator confirms the payload. A read-only probe that
// silently demotes healthy exits would invert the pool's quality ranking for
// real traffic.
func markProbeComplete(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	if marker, ok := response.Body.(interface{ MarkComplete() }); ok {
		marker.MarkComplete()
	}
}

func (s *Service) testOpenCodeFreeModel(ctx context.Context, model Model) error {
	if s.opencodefree == nil {
		return ErrProviderNotConfigured
	}
	body, err := json.Marshal(map[string]any{
		"model":    model.UpstreamID,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
		// ocfProbeMaxTokens, not 1: free-tier models spend their whole budget on
		// invisible template tokens before emitting visible text, so a tiny
		// answer comes back 200 with empty content and fails payload validation.
		// The detailed OCF probes share this budget.
		"max_tokens": ocfProbeMaxTokens,
	})
	if err != nil {
		return err
	}
	response, err := s.opencodefree.Chat(ctx, runtimeconfig.Snapshot{}, body, false)
	if err != nil {
		var proxyErr *xkproxy.Error
		if errors.As(err, &proxyErr) {
			return err
		}
		return ErrUpstreamUnreachable
	}
	if response == nil || response.Body == nil {
		return ErrUpstreamUnreachable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return probeStatusError(response.StatusCode)
	}
	if _, err := nvidia.ValidateNonstreamChat(response); err != nil {
		return probeValidationError(err)
	}
	return nil
}

// probeStatusError classifies a non-2xx probe answer. Throttling, request
// timeouts and server-side failures may clear on another NVIDIA key or another
// proxy exit, so they become retryable. Everything else (notably 404 for a
// retired model and 401/403 for a bad credential) is a verdict about the target
// itself: replaying it would only burn upstream quota.
func probeStatusError(status int) error {
	switch {
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return ErrUpstreamUnreachable
	case status >= http.StatusInternalServerError:
		return ErrUpstreamUnreachable
	default:
		return ErrManualTestRequired
	}
}

// probeValidationError keeps an empty completion retryable. The OpenCodeFree
// gateway is known to answer HTTP 200 with no usable output during a network
// hiccup, while a malformed payload is a protocol verdict that will not change
// on a retry.
func probeValidationError(err error) error {
	if errors.Is(err, nvidia.ErrEmptyResponse) {
		return ErrUpstreamUnreachable
	}
	return ErrManualTestRequired
}

// probeWAV is a minimal valid mono PCM WAV containing one silent sample. It is
// deliberately generated in memory so verification never reads a caller file.
func isAudioContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return strings.HasPrefix(mediaType, "audio/") || mediaType == "application/octet-stream"
}

var probeWAV = []byte{
	'R', 'I', 'F', 'F', 0x26, 0x00, 0x00, 0x00, 'W', 'A', 'V', 'E',
	'f', 'm', 't', ' ', 0x10, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00,
	0x40, 0x1f, 0x00, 0x00, 0x80, 0x3e, 0x00, 0x00, 0x02, 0x00, 0x10, 0x00,
	'd', 'a', 't', 'a', 0x02, 0x00, 0x00, 0x00, 0x00, 0x00,
}

func (s *Service) ListEnabled(ctx context.Context) ([]Model, error) {
	return s.repository.ListEnabled(ctx)
}

func (s *Service) Resolve(ctx context.Context, publicID string, requirements Requirements) (Model, error) {
	model, err := s.repository.ResolveEnabled(ctx, publicID)
	if err != nil {
		return Model{}, fmt.Errorf("resolve model %q: %w", publicID, err)
	}
	if model.Provider == "" {
		model.Provider = defaultModelProvider
	}
	if err := validateRequirements(model, requirements); err != nil {
		return Model{}, fmt.Errorf("validate model %q capabilities: %w", publicID, err)
	}
	return model, nil
}

func (s *Service) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	if err := s.repository.SetEnabled(ctx, id, enabled, s.clock.Now()); err != nil {
		return fmt.Errorf("set model enabled state: %w", err)
	}
	return nil
}

// CountUnexpressibleReasoningProfiles reports how many enabled reasoning
// models have a profile that cannot express any level — the llama shape
// (levels=[none], zero_allowed=false) that made every reasoning_effort request
// fail 501 model_capability_unsupported. The count is advisory: startup logs it
// per model so an operator can PATCH the row; nothing is auto-written.
func (s *Service) CountUnexpressibleReasoningProfiles(ctx context.Context) (int, []string, error) {
	models, err := s.repository.List(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("list models for reasoning profile check: %w", err)
	}
	var broken []string
	for _, model := range models {
		// The check covers every reasoning model, not only enabled ones: a
		// disabled row with an unexpressible profile will 501 the moment an
		// operator re-enables it.
		if !model.SupportsReasoning {
			continue
		}
		if len(compat.AvailableLevels(model.ReasoningProfile())) == 0 {
			broken = append(broken, model.PublicID)
		}
	}
	return len(broken), broken, nil
}

func (s *Service) SetCapabilityVerified(ctx context.Context, id int64, verifiedAt *time.Time) error {
	if err := s.repository.SetCapabilityVerified(ctx, id, verifiedAt, s.clock.Now()); err != nil {
		return fmt.Errorf("set model capability verification: %w", err)
	}
	return nil
}

func (s *Service) BlockKeyModel(ctx context.Context, keyID, modelID int64, reason string, upstreamStatus *int) error {
	if err := s.repository.BlockKeyModel(ctx, keyID, modelID, reason, upstreamStatus, s.clock.Now()); err != nil {
		return fmt.Errorf("block NVIDIA key for model: %w", err)
	}
	return nil
}

func (s *Service) UnblockKeyModel(ctx context.Context, keyID, modelID int64, manualTestSucceeded bool) error {
	if !manualTestSucceeded {
		return ErrManualTestRequired
	}
	if err := s.repository.UnblockKeyModel(ctx, keyID, modelID); err != nil {
		return fmt.Errorf("unblock NVIDIA key for model: %w", err)
	}
	return nil
}

func (s *Service) DeleteModel(ctx context.Context, id int64) error {
	if err := s.repository.DeleteModel(ctx, id); err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	return nil
}

// SyncOpenCodeFreeModels disables enabled OpenCodeFree models whose upstream ID
// is no longer present in the gateway's /models list. It is idempotent and
// safe to run periodically: a gateway fetch failure aborts without disabling
// anything, and only models with provider=opencodefree and enabled=1 are
// considered. New gateway models are not auto-enabled; operators enable them
// via the normal selection flow.
func (s *Service) SyncOpenCodeFreeModels(ctx context.Context) (int, error) {
	if isNilOpenCodeFreeClient(s.opencodefree) {
		return 0, nil
	}
	gatewayIDs, err := s.opencodefree.Models(ctx)
	if err != nil {
		return 0, fmt.Errorf("fetch OpenCodeFree gateway models: %w", err)
	}
	gatewaySet := make(map[string]struct{}, len(gatewayIDs))
	for _, id := range gatewayIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" || !isFreeModelID(trimmed) {
			continue
		}
		gatewaySet[trimmed] = struct{}{}
	}
	models, err := s.repository.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list models for OpenCodeFree sync: %w", err)
	}
	disabled := 0
	for _, model := range models {
		if model.Provider != ProviderOpenCodeFree || !model.Enabled {
			continue
		}
		if _, ok := gatewaySet[model.UpstreamID]; ok {
			continue
		}
		// Also check public ID without prefix as a fallback; some legacy rows
		// may have stored the full public ID as upstream (defensive).
		if _, ok := gatewaySet[strings.TrimPrefix(model.PublicID, ProviderOpenCodeFree+"/")]; ok {
			continue
		}
		if err := s.repository.SetEnabled(ctx, model.ID, false, s.clock.Now()); err != nil {
			slog.Default().Warn("auto-disable stale OpenCodeFree model failed", "public_id", model.PublicID, "upstream_id", model.UpstreamID, "error", err)
			continue
		}
		slog.Default().Info("auto-disabled stale OpenCodeFree model", "public_id", model.PublicID, "upstream_id", model.UpstreamID)
		disabled++
	}
	return disabled, nil
}

// StartOpenCodeFreeSync runs a background loop that periodically calls
// SyncOpenCodeFreeModels. It stops when ctx is canceled. The interval is
// deliberately long (1h) because the gateway's free list changes at most daily
// and each run touches the DB for every enabled free model.
func (s *Service) StartOpenCodeFreeSync(ctx context.Context, interval time.Duration) {
	if isNilOpenCodeFreeClient(s.opencodefree) {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		// Initial jitter so a fleet restart does not hammer the gateway at once.
		jitter := time.Duration(0)
		if interval > time.Minute {
			jitter = time.Duration(float64(interval) * 0.1 * float64(time.Now().UnixNano()%10) / 10)
		}
		timer := time.NewTimer(jitter)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// First sync shortly after startup with a timeout so a hung gateway does
		// not block shutdown.
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := s.SyncOpenCodeFreeModels(syncCtx); err != nil {
			slog.Default().Warn("initial OpenCodeFree sync failed", "error", err)
		}
		cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if _, err := s.SyncOpenCodeFreeModels(syncCtx); err != nil {
					slog.Default().Warn("periodic OpenCodeFree sync failed", "error", err)
				}
				cancel()
			}
		}
	}()
}

func candidateFromHint(modelID string, hint nvidia.CapabilityHint) Candidate {
	return Candidate{
		PublicID:                modelID,
		UpstreamID:              modelID,
		DisplayName:             modelID,
		Kind:                    Kind(hint.Kind),
		Provider:                ProviderNVIDIA,
		Channel:                 ProviderNVIDIA,
		Badge:                   "NVIDIA",
		Status:                  "available",
		Capabilities:            capabilityTags(Kind(hint.Kind), hint.SupportsVision, hint.SupportsTools, hint.SupportsReasoning),
		SupportsVision:          hint.SupportsVision,
		SupportsTools:           hint.SupportsTools,
		ToolsStatus:             toolStatusForCapabilities(hint.SupportsTools),
		SupportsReasoning:       hint.SupportsReasoning,
		ReasoningStatus:         reasoningStatusForCapabilities(hint.SupportsReasoning),
		ReasoningWireFormat:     string(hint.ReasoningWireFormat),
		ReasoningLevels:         defaultReasoningLevelsForCandidate(hint.SupportsReasoning),
		ReasoningMaxBudget:      128000,
		ReasoningZeroAllowed:    hint.SupportsReasoning,
		ReasoningDynamicAllowed: hint.SupportsReasoning,
	}
}

func defaultReasoningLevelsForCandidate(supported bool) []string {
	if !supported {
		return nil
	}
	return defaultReasoningLevels()
}

func reasoningStatusForCapabilities(supported bool) string {
	if supported {
		return ReasoningStatusInferred
	}
	return ReasoningStatusUnknown
}

func toolStatusForCapabilities(supported bool) string {
	if supported {
		return ToolsStatusInferred
	}
	return ToolsStatusUnknown
}

func candidateFromOpenCodeFree(modelID string) Candidate {
	hint := openCodeFreeCapabilityHint(modelID)
	return Candidate{
		PublicID:                ProviderOpenCodeFree + "/" + modelID,
		UpstreamID:              modelID,
		DisplayName:             modelID,
		Kind:                    KindChat,
		Provider:                ProviderOpenCodeFree,
		Channel:                 ProviderOpenCodeFree,
		Badge:                   "OpenCodeFree",
		Status:                  "available",
		Capabilities:            capabilityTags(KindChat, hint.SupportsVision, hint.SupportsTools, hint.SupportsReasoning),
		SupportsVision:          hint.SupportsVision,
		SupportsTools:           hint.SupportsTools,
		ToolsStatus:             toolStatusForCapabilities(hint.SupportsTools),
		SupportsReasoning:       hint.SupportsReasoning,
		ReasoningStatus:         hint.ReasoningStatus,
		ReasoningWireFormat:     hint.ReasoningWire,
		ReasoningLevels:         defaultReasoningLevelsForCandidate(hint.SupportsReasoning),
		ReasoningMaxBudget:      128000,
		ReasoningZeroAllowed:    hint.SupportsReasoning,
		ReasoningDynamicAllowed: hint.SupportsReasoning,
	}
}

func capabilityTags(kind Kind, vision, tools, reasoning bool) []string {
	tags := []string{string(kind)}
	if vision {
		tags = append(tags, "vision")
	}
	if tools {
		tags = append(tags, "tools")
	}
	if reasoning {
		tags = append(tags, "reasoning")
	}
	return tags
}
