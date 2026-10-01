package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nvidia-router/internal/keystate"
	"nvidia-router/internal/modelcatalog"
	"nvidia-router/internal/xkproxy"
)

type fakeModels struct {
	candidates       []modelcatalog.Candidate
	models           []modelcatalog.Model
	saved            []modelcatalog.Selection
	saveResult       *modelcatalog.MutationResult
	discoverKey      int64
	cleared          [2]int64
	deleteCalled     int64
	deleteErr        error
	patchErr         error
	saveErr          error
	listErr          error
	discoverErr      error
	verifyErr        error
	listCalls        int
	candidateTested  []string
	candidateTestErr error
}

func (f *fakeModels) TestCandidate(_ context.Context, provider, upstreamID string) error {
	f.candidateTested = append(f.candidateTested, provider+"/"+upstreamID)
	return f.candidateTestErr
}

func (f *fakeModels) DeleteModel(_ context.Context, id int64) error {
	f.deleteCalled = id
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return nil
}

func (f *fakeModels) DiscoverCandidates(_ context.Context, keyID int64) ([]modelcatalog.Candidate, error) {
	f.discoverKey = keyID
	if f.discoverErr != nil {
		return nil, f.discoverErr
	}
	return f.candidates, nil
}
func (f *fakeModels) SaveSelection(_ context.Context, s []modelcatalog.Selection) error {
	f.saved = append([]modelcatalog.Selection(nil), s...)
	return nil
}
func (f *fakeModels) SaveSelectionResult(_ context.Context, s []modelcatalog.Selection) (modelcatalog.MutationResult, error) {
	f.saved = append([]modelcatalog.Selection(nil), s...)
	if f.saveErr != nil {
		return modelcatalog.MutationResult{}, f.saveErr
	}
	if f.saveResult != nil {
		return *f.saveResult, nil
	}
	return modelcatalog.MutationResult{Models: append([]modelcatalog.Model(nil), f.models...), PreviousKinds: map[int64]modelcatalog.Kind{}}, nil
}

func TestModelAPIRejectsUnsupportedModelKind(t *testing.T) {
	handler := NewModels(&fakeModels{saveErr: modelcatalog.ErrInvalidModelSelection}, fakeCandidateKeys{}, &fakeStateSync{})
	response := performAdminRequest(handler, http.MethodPost, "/admin/api/models", `{"models":[{"public_id":"model","upstream_id":"vendor/model","display_name":"Model","kind":"unsupported"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
		t.Fatalf("unsupported kind status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelAPIRejectsEmptyModelDisplayName(t *testing.T) {
	handler := NewModels(&fakeModels{saveErr: modelcatalog.ErrInvalidModelSelection}, fakeCandidateKeys{}, &fakeStateSync{})
	response := performAdminRequest(handler, http.MethodPost, "/admin/api/models", `{"models":[{"public_id":"model","upstream_id":"vendor/model","display_name":"","kind":"chat"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
		t.Fatalf("empty display name status=%d body=%s", response.Code, response.Body.String())
	}
}
func (f *fakeModels) List(context.Context) ([]modelcatalog.Model, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]modelcatalog.Model(nil), f.models...), nil
}
func (f *fakeModels) Patch(_ context.Context, id int64, p modelcatalog.Patch) (modelcatalog.Model, error) {
	if f.patchErr != nil {
		return modelcatalog.Model{}, f.patchErr
	}
	for _, m := range f.models {
		if m.ID == id {
			if p.Enabled != nil && *p.Enabled && (m.Kind == modelcatalog.KindASR || m.Kind == modelcatalog.KindTTS) && m.CapabilityVerifiedAt == nil {
				return modelcatalog.Model{}, modelcatalog.ErrCapabilityUnverified
			}
			if p.Enabled != nil {
				m.Enabled = *p.Enabled
			}
			if p.Kind != nil {
				m.Kind = *p.Kind
			}
			return m, nil
		}
	}
	return modelcatalog.Model{}, modelcatalog.ErrModelNotFound
}
func (f *fakeModels) PatchResult(ctx context.Context, id int64, p modelcatalog.Patch) (modelcatalog.Model, modelcatalog.Kind, error) {
	if f.patchErr != nil {
		return modelcatalog.Model{}, "", f.patchErr
	}
	for _, m := range f.models {
		if m.ID == id {
			updated, err := f.Patch(ctx, id, p)
			return updated, m.Kind, err
		}
	}
	return modelcatalog.Model{}, "", modelcatalog.ErrModelNotFound
}

func TestModelAPIMapsUnexpectedMutationErrorsToInternalError(t *testing.T) {
	handler := NewModels(&fakeModels{patchErr: errors.New("database unavailable")}, fakeCandidateKeys{}, &fakeStateSync{})
	response := performAdminRequest(handler, http.MethodPatch, "/admin/api/models/9", `{"enabled":true}`)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "database unavailable") {
		t.Fatalf("unexpected mutation status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelAPIMapsVersionConflictToConflict(t *testing.T) {
	handler := NewModels(&fakeModels{patchErr: modelcatalog.ErrModelVersionConflict}, fakeCandidateKeys{}, &fakeStateSync{})
	response := performAdminRequest(handler, http.MethodPatch, "/admin/api/models/9", `{"enabled":true}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "model_version_conflict") {
		t.Fatalf("version conflict status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelAPIUsesMutationResultsForDirectPoolSync(t *testing.T) {
	models := &fakeModels{
		models:  []modelcatalog.Model{{ID: 9, PublicID: "model", Kind: modelcatalog.KindChat, Enabled: false}},
		listErr: errors.New("list must not be called for mutation sync"),
		saveResult: &modelcatalog.MutationResult{
			Models:        []modelcatalog.Model{{ID: 9, PublicID: "model", Kind: modelcatalog.KindEmbedding, Enabled: false}},
			PreviousKinds: map[int64]modelcatalog.Kind{9: modelcatalog.KindChat},
		},
	}
	syncer := &modelSyncFake{}
	handler := NewModels(models, fakeCandidateKeys{}, syncer)

	response := performAdminRequest(handler, http.MethodPost, "/admin/api/models", `{"models":[{"public_id":"model","upstream_id":"vendor/model","display_name":"Model","kind":"embedding"}]}`)
	if response.Code != http.StatusOK || syncer.enabled[9] || !containsInt64(syncer.clearedModels, 9) || models.listCalls != 0 {
		t.Fatalf("save sync status=%d enabled=%v cleared=%v body=%s", response.Code, syncer.enabled, syncer.clearedModels, response.Body.String())
	}

	response = performAdminRequest(handler, http.MethodPatch, "/admin/api/models/9", `{"kind":"embedding","enabled":true}`)
	if response.Code != http.StatusOK || !syncer.enabled[9] || len(syncer.clearedModels) != 2 || models.listCalls != 0 {
		t.Fatalf("patch sync status=%d enabled=%v cleared=%v body=%s", response.Code, syncer.enabled, syncer.clearedModels, response.Body.String())
	}
}

func TestModelAPIOrdersMutationAndPoolSync(t *testing.T) {
	models := &orderedMutationModels{}
	syncer := newBlockingModelSync()
	handler := NewModels(models, fakeCandidateKeys{}, syncer)

	aDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		aDone <- performAdminRequest(handler, http.MethodPatch, "/admin/api/models/9", `{"enabled":false}`)
	}()

	select {
	case <-syncer.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("request A did not reach Pool sync")
	}

	bDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		bDone <- performAdminRequest(handler, http.MethodPatch, "/admin/api/models/9", `{"enabled":true}`)
	}()

	select {
	case response := <-bDone:
		t.Fatalf("request B completed before request A sync was released: status=%d", response.Code)
	case <-time.After(100 * time.Millisecond):
	}

	close(syncer.releaseFirst)
	if response := <-aDone; response.Code != http.StatusOK {
		t.Fatalf("request A status = %d, want %d", response.Code, http.StatusOK)
	}
	if response := <-bDone; response.Code != http.StatusOK {
		t.Fatalf("request B status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := syncer.currentEnabled(); !got {
		t.Fatal("Pool ended disabled after the later enabled mutation")
	}
}

type orderedMutationModels struct{ fakeModels }

func (m *orderedMutationModels) PatchResult(_ context.Context, id int64, patch modelcatalog.Patch) (modelcatalog.Model, modelcatalog.Kind, error) {
	enabled := false
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}
	return modelcatalog.Model{ID: id, Kind: modelcatalog.KindChat, Enabled: enabled}, modelcatalog.KindChat, nil
}

type blockingModelSync struct {
	mu            sync.Mutex
	enabled       bool
	firstStarted  chan struct{}
	releaseFirst  chan struct{}
	firstCallOnce sync.Once
}

func newBlockingModelSync() *blockingModelSync {
	return &blockingModelSync{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
}

func (s *blockingModelSync) SetModelEnabled(_ int64, enabled bool) {
	if !enabled {
		s.firstCallOnce.Do(func() { close(s.firstStarted) })
		<-s.releaseFirst
	}
	s.mu.Lock()
	s.enabled = enabled
	s.mu.Unlock()
}

func (*blockingModelSync) ClearModelBlocks(int64)           {}
func (*blockingModelSync) SetModelBlock(int64, int64, bool) {}

func (s *blockingModelSync) currentEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func TestModelAPISyncsNewModelWithoutClearingBlocks(t *testing.T) {
	models := &fakeModels{
		listErr: errors.New("list must not be called for mutation sync"),
		saveResult: &modelcatalog.MutationResult{
			Models:        []modelcatalog.Model{{ID: 10, PublicID: "new-model", Kind: modelcatalog.KindChat, Enabled: true}},
			PreviousKinds: map[int64]modelcatalog.Kind{},
		},
	}
	syncer := &modelSyncFake{}
	handler := NewModels(models, fakeCandidateKeys{}, syncer)

	response := performAdminRequest(handler, http.MethodPost, "/admin/api/models", `{"models":[{"public_id":"new-model","upstream_id":"vendor/new-model","display_name":"New model","kind":"chat","enabled":true}]}`)
	if response.Code != http.StatusOK || !syncer.enabled[10] || len(syncer.clearedModels) != 0 || models.listCalls != 0 {
		t.Fatalf("new model sync status=%d enabled=%v cleared=%v listCalls=%d body=%s", response.Code, syncer.enabled, syncer.clearedModels, models.listCalls, response.Body.String())
	}
}

func containsInt64(values []int64, want int64) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type modelSyncFake struct {
	enabled       map[int64]bool
	clearedModels []int64
}

func (*modelSyncFake) LoadSnapshot([]keystate.KeySnapshot, []keystate.ModelBlock) {}
func (*modelSyncFake) UpsertKey(keystate.KeySnapshot)                             {}
func (*modelSyncFake) SetKeyEnabled(int64, bool)                                  {}
func (*modelSyncFake) RemoveKey(int64)                                            {}
func (*modelSyncFake) ApplySuccess(int64)                                         {}
func (*modelSyncFake) ApplyFailure(int64, int64, interface{}, keystate.KeySnapshot) {
}
func (s *modelSyncFake) SetModelBlock(int64, int64, bool) {}
func (s *modelSyncFake) SetModelEnabled(id int64, enabled bool) {
	if s.enabled == nil {
		s.enabled = make(map[int64]bool)
	}
	s.enabled[id] = enabled
}
func (s *modelSyncFake) ClearModelBlocks(id int64) {
	s.clearedModels = append(s.clearedModels, id)
}

func (f *fakeModels) VerifyAndUnblock(_ context.Context, keyID, modelID int64) (modelcatalog.Model, error) {
	if f.verifyErr != nil {
		return modelcatalog.Model{}, f.verifyErr
	}
	f.cleared = [2]int64{keyID, modelID}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	return modelcatalog.Model{ID: modelID, CapabilityVerifiedAt: &now}, nil
}

type fakeCandidateKeys struct {
	id  int64
	err error
}

func (f fakeCandidateKeys) FirstEnabledID(context.Context) (int64, error) { return f.id, f.err }
func (f fakeCandidateKeys) CountEnabled(context.Context) (int, error)     { return 0, nil }

func TestModelAPIUsesFirstKeyAndEnforcesAudioVerification(t *testing.T) {
	models := &fakeModels{candidates: []modelcatalog.Candidate{{UpstreamID: "vendor/model", DisplayName: "Model", Kind: modelcatalog.KindChat}}, models: []modelcatalog.Model{{ID: 9, PublicID: "speech", UpstreamID: "vendor/speech", DisplayName: "Speech", Kind: modelcatalog.KindTTS, BlockedByKeyIDs: []int64{5}}}}
	syncer := &fakeStateSync{}
	handler := NewModels(models, fakeCandidateKeys{id: 5}, syncer)
	listResponse := performAdminRequest(handler, http.MethodGet, "/admin/api/models", "")
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), `"blocked_by_key_ids":[5]`) {
		t.Fatalf("model list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	response := performAdminRequest(handler, http.MethodGet, "/admin/api/models/candidates", "")
	if response.Code != http.StatusOK || models.discoverKey != 5 || !strings.Contains(response.Body.String(), "vendor/model") {
		t.Fatalf("candidates status=%d key=%d body=%s", response.Code, models.discoverKey, response.Body.String())
	}
	response = performAdminRequest(handler, http.MethodPatch, "/admin/api/models/9", `{"enabled":true}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "capability_unverified") {
		t.Fatalf("audio gate status=%d body=%s", response.Code, response.Body.String())
	}
	response = performAdminRequest(handler, http.MethodDelete, "/admin/api/key-model-blocks/5/9", "")
	if response.Code != http.StatusOK || models.cleared != [2]int64{5, 9} || len(syncer.blocks) != 1 || syncer.blocks[0] != [3]int64{5, 9, 0} {
		t.Fatalf("unblock status=%d cleared=%v blocks=%v body=%s", response.Code, models.cleared, syncer.blocks, response.Body.String())
	}
}

func TestModelAPIExposesCandidateChannelMetadataAndPreservesProviderOnSave(t *testing.T) {
	models := &fakeModels{candidates: []modelcatalog.Candidate{{
		PublicID: "opencodefree/model-free", UpstreamID: "model-free", DisplayName: "Model Free",
		Kind: modelcatalog.KindChat, Provider: modelcatalog.ProviderOpenCodeFree,
		Channel: modelcatalog.ProviderOpenCodeFree, Badge: "OpenCodeFree", Status: "pending",
		Capabilities: []string{"chat", "free"},
	}}}
	handler := NewModels(models, fakeCandidateKeys{id: 5}, &fakeStateSync{})

	response := performAdminRequest(handler, http.MethodGet, "/admin/api/models/candidates", "")
	if response.Code != http.StatusOK {
		t.Fatalf("candidate status = %d, body = %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data []struct {
			PublicID     string   `json:"public_id"`
			Provider     string   `json:"provider"`
			Channel      string   `json:"channel"`
			Badge        string   `json:"badge"`
			Status       string   `json:"status"`
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode candidates: %v", err)
	}
	if len(envelope.Data) != 1 || envelope.Data[0].PublicID != "opencodefree/model-free" || envelope.Data[0].Provider != modelcatalog.ProviderOpenCodeFree || envelope.Data[0].Channel != modelcatalog.ProviderOpenCodeFree || envelope.Data[0].Badge != "OpenCodeFree" || envelope.Data[0].Status != "pending" {
		t.Fatalf("candidate metadata = %+v", envelope.Data)
	}
	if strings.Join(envelope.Data[0].Capabilities, ",") != "chat,free" {
		t.Fatalf("candidate capabilities = %v", envelope.Data[0].Capabilities)
	}

	response = performAdminRequest(handler, http.MethodPost, "/admin/api/models", `{"models":[{"public_id":"opencodefree/model-free","upstream_id":"model-free","display_name":"Model Free","kind":"chat","provider":"opencodefree","enabled":false}]}`)
	if response.Code != http.StatusOK || len(models.saved) != 1 || models.saved[0].Provider != modelcatalog.ProviderOpenCodeFree {
		t.Fatalf("save status = %d, saved = %+v, body = %s", response.Code, models.saved, response.Body.String())
	}
}

type configuredOpenCodeFreeModels struct{ *fakeModels }

func (*configuredOpenCodeFreeModels) OpenCodeFreeConfigured() bool { return true }

func TestModelCandidatesFallsBackToOpenCodeFreeWhenNoNVIDIAKeyExists(t *testing.T) {
	models := &configuredOpenCodeFreeModels{fakeModels: &fakeModels{candidates: []modelcatalog.Candidate{{
		PublicID: "opencodefree/model-free", UpstreamID: "model-free", DisplayName: "Model Free", Kind: modelcatalog.KindChat,
	}}}}
	handler := NewModels(models, fakeCandidateKeys{err: fmt.Errorf("no enabled key: %w", sql.ErrNoRows)}, &fakeStateSync{})

	response := performAdminRequest(handler, http.MethodGet, "/admin/api/models/candidates", "")
	if response.Code != http.StatusOK || models.discoverKey != 0 {
		t.Fatalf("fallback status = %d, discover key = %d, body = %s", response.Code, models.discoverKey, response.Body.String())
	}
}

func TestModelAPIMapsProxyErrorsToBadGateway(t *testing.T) {
	proxyErr := xkproxy.NewTransportError(errors.New("private proxy cause"))
	handler := NewModels(&fakeModels{discoverErr: proxyErr, verifyErr: proxyErr}, fakeCandidateKeys{id: 5}, &fakeStateSync{})

	candidates := performAdminRequest(handler, http.MethodGet, "/admin/api/models/candidates", "")
	if candidates.Code != http.StatusBadGateway || !strings.Contains(candidates.Body.String(), "upstream_proxy_unavailable") {
		t.Fatalf("candidate proxy status=%d body=%s", candidates.Code, candidates.Body.String())
	}

	verification := performAdminRequest(handler, http.MethodPost, "/admin/api/models/9/test", `{"key_id":5}`)
	if verification.Code != http.StatusBadGateway || !strings.Contains(verification.Body.String(), "upstream_proxy_unavailable") {
		t.Fatalf("verification proxy status=%d body=%s", verification.Code, verification.Body.String())
	}
}

func TestModelVerificationEndpointRequiresAllowlistedKeyIDAndSyncsOnlyOnSuccess(t *testing.T) {
	models := &fakeModels{models: []modelcatalog.Model{{ID: 9, PublicID: "speech", Kind: modelcatalog.KindTTS}}}
	syncer := &fakeStateSync{}
	handler := NewModels(models, fakeCandidateKeys{}, syncer)

	response := performAdminRequest(handler, http.MethodPost, "/admin/api/models/9/test", `{"key_id":5,"verified_at":"2026-07-31T00:00:00Z"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
		t.Fatalf("unknown field status=%d body=%s", response.Code, response.Body.String())
	}
	if models.cleared != [2]int64{} || len(syncer.blocks) != 0 {
		t.Fatalf("failed verification changed state: cleared=%v blocks=%v", models.cleared, syncer.blocks)
	}

	response = performAdminRequest(handler, http.MethodPost, "/admin/api/models/9/test", `{"key_id":5}`)
	if response.Code != http.StatusOK || models.cleared != [2]int64{5, 9} || len(syncer.blocks) != 1 || syncer.blocks[0] != [3]int64{5, 9, 0} {
		t.Fatalf("verification status=%d cleared=%v blocks=%v body=%s", response.Code, models.cleared, syncer.blocks, response.Body.String())
	}
}

func TestModelVerificationEndpointRejectsMalformedRoutesAndKeyIDs(t *testing.T) {
	handler := NewModels(&fakeModels{}, fakeCandidateKeys{}, &fakeStateSync{})
	for _, path := range []string{"/admin/api/models/9", "/admin/api/models/9/other", "/admin/api/models/0/test"} {
		response := performAdminRequest(handler, http.MethodPost, path, `{"key_id":5}`)
		if response.Code != http.StatusNotFound {
			t.Fatalf("path %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	response := performAdminRequest(handler, http.MethodPost, "/admin/api/models/9/test", `{"key_id":0}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
		t.Fatalf("key id status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelCandidatesReportsMissingKeyWithoutSecret(t *testing.T) {
	handler := NewModels(&fakeModels{}, fakeCandidateKeys{err: errors.New("no key")}, &fakeStateSync{})
	response := performAdminRequest(handler, http.MethodGet, "/admin/api/models/candidates", "")
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "no key") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelAPIDeletesModelAndSyncsState(t *testing.T) {
	models := &fakeModels{models: []modelcatalog.Model{{ID: 9, PublicID: "test-model"}}}
	syncer := &modelSyncFake{}
	handler := NewModels(models, fakeCandidateKeys{}, syncer)

	response := performAdminRequest(handler, http.MethodDelete, "/admin/api/models/9", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete model status=%d body=%s", response.Code, response.Body.String())
	}
	if models.deleteCalled != 9 {
		t.Fatalf("expected delete called with id 9, got %d", models.deleteCalled)
	}
	if syncer.enabled[9] != false {
		t.Fatalf("expected model 9 disabled in sync, got %v", syncer.enabled[9])
	}
	if !containsInt64(syncer.clearedModels, 9) {
		t.Fatalf("expected model 9 blocks cleared in sync, got %v", syncer.clearedModels)
	}
}

func TestModelCandidateTestEndpointRunsReadOnlyProbe(t *testing.T) {
	service := &fakeModels{}
	handler := NewModels(service, fakeCandidateKeys{}, &fakeStateSync{})

	response := performAdminRequest(handler, http.MethodPost, "/admin/api/models/candidates/test", `{"provider":"nvidia","upstream_id":"vendor/fresh"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Status     string `json:"status"`
		DurationMS int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Status != "success" || result.DurationMS < 0 {
		t.Fatalf("result = %+v, want success with a non-negative duration", result)
	}
	if len(service.candidateTested) != 1 || service.candidateTested[0] != "nvidia/vendor/fresh" {
		t.Fatalf("probed = %v, want [nvidia/vendor/fresh]", service.candidateTested)
	}
}

func TestModelCandidateTestEndpointReportsFailureReason(t *testing.T) {
	handler := NewModels(&fakeModels{candidateTestErr: modelcatalog.ErrNVIDIAKeyRequired}, fakeCandidateKeys{}, &fakeStateSync{})

	response := performAdminRequest(handler, http.MethodPost, "/admin/api/models/candidates/test", `{"provider":"nvidia","upstream_id":"vendor/fresh"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"status":"failed"`) || !strings.Contains(body, safeModelTestError(modelcatalog.ErrNVIDIAKeyRequired)) {
		t.Fatalf("body = %s, want failed with the safe error text", body)
	}
}

func TestModelCandidateTestEndpointRejectsInvalidInput(t *testing.T) {
	handler := NewModels(&fakeModels{}, fakeCandidateKeys{}, &fakeStateSync{})
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown provider", body: `{"provider":"legacy","upstream_id":"vendor/x"}`},
		{name: "missing provider", body: `{"upstream_id":"vendor/x"}`},
		{name: "empty upstream id", body: `{"provider":"nvidia","upstream_id":"  "}`},
		{name: "control character", body: "{\"provider\":\"nvidia\",\"upstream_id\":\"vendor\\n/x\"}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performAdminRequest(handler, http.MethodPost, "/admin/api/models/candidates/test", test.body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
