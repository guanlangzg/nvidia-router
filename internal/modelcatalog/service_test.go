package modelcatalog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"nvidia-router/internal/database"
	"nvidia-router/internal/runtimeconfig"
	"nvidia-router/internal/upstream/nvidia"
	"nvidia-router/internal/xkproxy"
)

func TestSaveSelectionRejectsInvalidModelFields(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	tests := []Selection{
		{PublicID: "unsupported", UpstreamID: "vendor/model", DisplayName: "Model", Kind: Kind("unsupported")},
		{PublicID: "empty-display", UpstreamID: "vendor/model", Kind: KindChat},
	}
	for _, selection := range tests {
		if err := service.SaveSelection(context.Background(), []Selection{selection}); !errors.Is(err, ErrInvalidModelSelection) {
			t.Errorf("selection %q error = %v, want ErrInvalidModelSelection", selection.PublicID, err)
		}
	}
}

func TestValidateNVIDIAKeyChecksSelectedCredentialWithoutCallingUpstream(t *testing.T) {
	service, _, secrets, discoverer := newCatalogTestService(t)
	if err := service.ValidateNVIDIAKey(context.Background(), 17); err != nil {
		t.Fatalf("ValidateNVIDIAKey: %v", err)
	}
	if secrets.lastKeyID != 17 {
		t.Fatalf("validated key id = %d, want 17", secrets.lastKeyID)
	}
	if discoverer.modelsCalls != 0 || discoverer.chatCalls != 0 {
		t.Fatalf("credential validation called upstream: models=%d chat=%d", discoverer.modelsCalls, discoverer.chatCalls)
	}
}

func TestModelPatchRejectsOpenAICompatibleProvider(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	result, err := service.SaveSelectionResult(context.Background(), []Selection{{
		PublicID: "provider-gate", UpstreamID: "vendor/provider-gate", DisplayName: "Provider gate", Kind: KindChat,
	}})
	if err != nil {
		t.Fatalf("SaveSelectionResult: %v", err)
	}
	provider := "openai_compatible"
	if _, err := service.Patch(context.Background(), result.Models[0].ID, Patch{Provider: &provider}); !errors.Is(err, ErrInvalidModelSelection) {
		t.Fatalf("Patch provider error = %v, want ErrInvalidModelSelection", err)
	}
}

func TestModelPatchRejectsEnablingExistingNonNVIDIAProvider(t *testing.T) {
	service, db, _, _ := newCatalogTestService(t)
	result, err := service.SaveSelectionResult(context.Background(), []Selection{{
		PublicID: "provider-enable-patch", UpstreamID: "vendor/provider-enable-patch", DisplayName: "Provider enable patch", Kind: KindChat,
	}})
	if err != nil {
		t.Fatalf("SaveSelectionResult: %v", err)
	}
	if _, err := db.Exec("UPDATE models SET provider = ? WHERE id = ?", "openai_compatible", result.Models[0].ID); err != nil {
		t.Fatalf("seed non-NVIDIA provider: %v", err)
	}
	enabled := true
	if _, err := service.Patch(context.Background(), result.Models[0].ID, Patch{Enabled: &enabled}); !errors.Is(err, ErrInvalidModelSelection) {
		t.Fatalf("Patch enabling non-NVIDIA model error = %v, want ErrInvalidModelSelection", err)
	}
	var storedEnabled int
	if err := db.QueryRow("SELECT enabled FROM models WHERE id = ?", result.Models[0].ID).Scan(&storedEnabled); err != nil {
		t.Fatalf("read model enabled state: %v", err)
	}
	if storedEnabled != 0 {
		t.Fatalf("stored enabled state = %d, want 0", storedEnabled)
	}
}

func TestSaveSelectionRejectsEnablingExistingNonNVIDIAProvider(t *testing.T) {
	service, db, _, _ := newCatalogTestService(t)
	result, err := service.SaveSelectionResult(context.Background(), []Selection{{
		PublicID: "provider-enable-selection", UpstreamID: "vendor/provider-enable-selection", DisplayName: "Provider enable selection", Kind: KindChat,
	}})
	if err != nil {
		t.Fatalf("SaveSelectionResult: %v", err)
	}
	if _, err := db.Exec("UPDATE models SET provider = ? WHERE id = ?", "openai_compatible", result.Models[0].ID); err != nil {
		t.Fatalf("seed non-NVIDIA provider: %v", err)
	}
	selection := Selection{
		PublicID: "provider-enable-selection", UpstreamID: "vendor/provider-enable-selection", DisplayName: "Provider enable selection", Kind: KindChat, Enabled: true,
	}
	if err := service.SaveSelection(context.Background(), []Selection{selection}); !errors.Is(err, ErrInvalidModelSelection) {
		t.Fatalf("SaveSelection enabling non-NVIDIA model error = %v, want ErrInvalidModelSelection", err)
	}
	var storedEnabled int
	if err := db.QueryRow("SELECT enabled FROM models WHERE id = ?", result.Models[0].ID).Scan(&storedEnabled); err != nil {
		t.Fatalf("read model enabled state: %v", err)
	}
	if storedEnabled != 0 {
		t.Fatalf("stored enabled state = %d, want 0", storedEnabled)
	}
}

func TestSetEnabledRejectsExistingNonNVIDIAProvider(t *testing.T) {
	service, db, _, _ := newCatalogTestService(t)
	result, err := service.SaveSelectionResult(context.Background(), []Selection{{
		PublicID: "provider-enable-set", UpstreamID: "vendor/provider-enable-set", DisplayName: "Provider enable set", Kind: KindChat,
	}})
	if err != nil {
		t.Fatalf("SaveSelectionResult: %v", err)
	}
	if _, err := db.Exec("UPDATE models SET provider = ? WHERE id = ?", "openai_compatible", result.Models[0].ID); err != nil {
		t.Fatalf("seed non-NVIDIA provider: %v", err)
	}
	if err := service.SetEnabled(context.Background(), result.Models[0].ID, true); !errors.Is(err, ErrInvalidModelSelection) {
		t.Fatalf("SetEnabled enabling non-NVIDIA model error = %v, want ErrInvalidModelSelection", err)
	}
}

func TestPatchRejectsInvalidModelSelectionSentinel(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	result, err := service.SaveSelectionResult(context.Background(), []Selection{{
		PublicID: "patch-invalid", UpstreamID: "vendor/patch-invalid", DisplayName: "Patch invalid", Kind: KindChat,
	}})
	if err != nil {
		t.Fatalf("SaveSelectionResult: %v", err)
	}
	invalid := Kind("unsupported")
	if _, err := service.Patch(context.Background(), result.Models[0].ID, Patch{Kind: &invalid}); !errors.Is(err, ErrInvalidModelSelection) {
		t.Fatalf("Patch error = %v, want ErrInvalidModelSelection", err)
	}
}

func TestSaveSelectionResultReportsPreviousKindOnlyForExistingModel(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	selection := Selection{PublicID: "mutation-result", UpstreamID: "vendor/mutation-result", DisplayName: "Mutation result", Kind: KindChat, Enabled: true}

	first, err := service.SaveSelectionResult(context.Background(), []Selection{selection})
	if err != nil {
		t.Fatalf("first SaveSelectionResult: %v", err)
	}
	if len(first.Models) != 1 {
		t.Fatalf("first result models = %d, want 1", len(first.Models))
	}
	modelID := first.Models[0].ID
	if _, exists := first.PreviousKinds[modelID]; exists {
		t.Fatalf("first result PreviousKinds[%d] exists = %q, want absent", modelID, first.PreviousKinds[modelID])
	}

	selection.Kind = KindEmbedding
	second, err := service.SaveSelectionResult(context.Background(), []Selection{selection})
	if err != nil {
		t.Fatalf("second SaveSelectionResult: %v", err)
	}
	if got := second.PreviousKinds[modelID]; got != KindChat {
		t.Fatalf("second result PreviousKinds[%d] = %q, want %q", modelID, got, KindChat)
	}
}

func TestDiscoverCandidatesDoesNotMutateWhitelist(t *testing.T) {
	service, db, secrets, discoverer := newCatalogTestService(t)
	discoverer.models = []string{"model-b", "model-a"}

	candidates, err := service.DiscoverCandidates(context.Background(), 11)
	if err != nil {
		t.Fatalf("DiscoverCandidates: %v", err)
	}
	// Both are NVIDIA candidates with no encoded size, so they sort by vendor
	// (ID prefix) rather than the upstream discovery order.
	if got := candidateIDs(candidates); !reflect.DeepEqual(got, []string{"model-a", "model-b"}) {
		t.Fatalf("candidate IDs = %v", got)
	}
	if secrets.lastKeyID != 11 || discoverer.lastToken != "secret-for-11" {
		t.Fatalf("secret/discovery = %d/%q", secrets.lastKeyID, discoverer.lastToken)
	}
	// The DeepSeek alias migration seeds one model; discovery must not add any.
	assertModelCount(t, db, 1)

	selected := Selection{PublicID: "public-a", UpstreamID: "model-a", DisplayName: "Model A", Kind: KindChat, Enabled: true}
	if err := service.SaveSelection(context.Background(), []Selection{selected}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	assertModelCount(t, db, 2)

	discoverer.models = []string{"new-model"}
	if _, err := service.DiscoverCandidates(context.Background(), 12); err != nil {
		t.Fatalf("DiscoverCandidates second key: %v", err)
	}
	assertModelCount(t, db, 2)
}

func TestDiscoverCandidatesPreservesProxyError(t *testing.T) {
	service, _, _, discoverer := newCatalogTestService(t)
	discoverer.modelsErr = xkproxy.NewTransportError(errors.New("private proxy cause"))

	_, err := service.DiscoverCandidates(context.Background(), 11)
	var proxyErr *xkproxy.Error
	if !errors.As(err, &proxyErr) {
		t.Fatalf("DiscoverCandidates error = %T %v, want *xkproxy.Error", err, err)
	}
}

func TestDiscoverCandidatesKeepsOnlyFreeOpenCodeFreeModels(t *testing.T) {
	service, _, _, discoverer := newCatalogTestService(t)
	discoverer.models = []string{"vendor/model-a"}
	gateway := &fakeOpenCodeFreeGateway{models: []string{"gpt-5", "hy3-free", "claude-opus-5", "NEMOTRON-3-ULTRA-FREE"}}
	service = service.WithOpenCodeFree(gateway)

	candidates, err := service.DiscoverCandidates(context.Background(), 11)
	if err != nil {
		t.Fatalf("DiscoverCandidates: %v", err)
	}
	// Only -free OpenCodeFree models become candidates, ahead of the NVIDIA
	// catalog; the gateway's non-free models never surface for selection.
	got := candidateIDs(candidates)
	want := []string{"hy3-free", "NEMOTRON-3-ULTRA-FREE", "vendor/model-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate IDs = %v, want %v", got, want)
	}
	for _, candidate := range candidates {
		if candidate.Provider == ProviderOpenCodeFree && candidate.Badge != "OpenCodeFree" {
			t.Errorf("OpenCodeFree candidate %q badge = %q", candidate.UpstreamID, candidate.Badge)
		}
	}
}

func TestWhitelistMapsPublicIDAndDisablesImmediately(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	selections := []Selection{
		{PublicID: "chat-public", UpstreamID: "vendor/chat", DisplayName: "Chat", Kind: KindChat, Enabled: true},
		{PublicID: "hidden", UpstreamID: "vendor/hidden", DisplayName: "Hidden", Kind: KindChat, Enabled: false},
	}
	if err := service.SaveSelection(context.Background(), selections); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}

	models, err := service.ListEnabled(context.Background())
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	// The DeepSeek seed migration (015) inserts an enabled model, so the enabled
	// set is the seed plus the one the test enables. Resolve by public id below,
	// not by position, because the seed sorts before "chat-public".
	chatModel, err := service.Resolve(context.Background(), "chat-public", Requirements{Kind: KindChat})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(models) < 2 {
		t.Fatalf("enabled models = %+v, want seed + chat-public", models)
	}
	if chatModel.UpstreamID != "vendor/chat" {
		t.Fatalf("upstream ID = %q", chatModel.UpstreamID)
	}

	if err := service.SetEnabled(context.Background(), chatModel.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if _, err := service.Resolve(context.Background(), "chat-public", Requirements{Kind: KindChat}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("Resolve disabled error = %v", err)
	}
}

func TestPatchConcurrentChangesPreserveBothFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "router.db")
	primaryDB, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Open primary DB: %v", err)
	}
	primaryDB.SetMaxOpenConns(2)
	t.Cleanup(func() {
		if err := primaryDB.Close(); err != nil {
			t.Errorf("Close primary DB: %v", err)
		}
	})
	blockerDB, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("Open blocker DB: %v", err)
	}
	t.Cleanup(func() {
		if err := blockerDB.Close(); err != nil {
			t.Errorf("Close blocker DB: %v", err)
		}
	})
	service := NewService(NewRepository(primaryDB), &fakeSecrets{}, &fakeDiscoverer{}, nvidia.DefaultDescriptor(), catalogClock{})
	// The migrations seed the deepseek-v4-flash alias; this test reasons about the
	// single model it inserts, so clear the seeded whitelist first.
	if _, err := primaryDB.Exec(`DELETE FROM models`); err != nil {
		t.Fatalf("clear seeded models: %v", err)
	}
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "concurrent", UpstreamID: "vendor/concurrent", DisplayName: "Original", Kind: KindChat,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	models, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	modelID := models[0].ID
	blockerTx, err := blockerDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("Begin blocker transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := blockerTx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("Rollback blocker transaction: %v", err)
		}
	})

	var displayName = "Updated name"
	var enabled = true
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, patchErr := service.Patch(context.Background(), modelID, Patch{DisplayName: &displayName})
		errs <- patchErr
	}()
	go func() {
		<-start
		_, patchErr := service.Patch(context.Background(), modelID, Patch{Enabled: &enabled})
		errs <- patchErr
	}()
	close(start)
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for primaryDB.Stats().InUse != 2 {
		select {
		case <-deadline.C:
			if err := blockerTx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("Rollback blocker transaction after deadline: %v", err)
			}
			for range 2 {
				<-errs
			}
			t.Fatal("PATCH calls did not both reach blocked transaction stage before deadline")
		case <-ticker.C:
		}
	}
	if err := blockerTx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("Rollback blocker transaction: %v", err)
	}
	patchErrors := make([]error, 0, 2)
	for range 2 {
		patchErrors = append(patchErrors, <-errs)
	}
	for _, patchErr := range patchErrors {
		if patchErr != nil {
			t.Fatalf("Patch: %v", patchErr)
		}
	}

	updated, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List updated model: %v", err)
	}
	if updated[0].DisplayName != displayName || !updated[0].Enabled {
		t.Fatalf("concurrent patch lost update: %+v", updated[0])
	}
}

func TestConcurrentModelUpdatesAdvanceRevision(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "revision-concurrent", UpstreamID: "vendor/revision-concurrent", DisplayName: "Revision", Kind: KindChat,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	model := modelByPublicID(t, service, "revision-concurrent")
	verifiedAt := time.Date(2026, 7, 30, 5, 0, 0, 0, time.UTC)
	const updatesPerMethod = 32
	start := make(chan struct{})
	errs := make(chan error, updatesPerMethod*2)
	for range updatesPerMethod {
		go func() {
			<-start
			errs <- service.SetEnabled(context.Background(), model.ID, false)
		}()
		go func() {
			<-start
			errs <- service.SetCapabilityVerified(context.Background(), model.ID, &verifiedAt)
		}()
	}
	close(start)
	for range updatesPerMethod * 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent model update: %v", err)
		}
	}

	updatedModel := modelByPublicID(t, service, "revision-concurrent")
	wantRevision := model.updatedAt.Add(updatesPerMethod * 2 * time.Nanosecond)
	if !updatedModel.updatedAt.Equal(wantRevision) {
		t.Fatalf("updated_at = %s, want %s after %d concurrent updates", updatedModel.updatedAt, wantRevision, updatesPerMethod*2)
	}
}

func TestVerifyAndUnblockMapsExistingModelConditionalUpdateMissToConflict(t *testing.T) {
	service, db, _, _ := newCatalogTestService(t)
	verifiedAt := time.Date(2026, 7, 30, 5, 0, 0, 0, time.UTC)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "conditional-conflict", UpstreamID: "vendor/conditional-conflict", DisplayName: "Conditional", Kind: KindTTS,
		Enabled: true, CapabilityVerifiedAt: &verifiedAt,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	model := modelByPublicID(t, service, "conditional-conflict")
	if _, err := db.Exec("UPDATE models SET updated_at = ? WHERE id = ?", "2026-07-30T04:00:00.000Z", model.ID); err != nil {
		t.Fatalf("rewrite model revision: %v", err)
	}

	_, verifyErr := service.repository.VerifyAndUnblock(context.Background(), 1, model.ID, model.updatedAt, verifiedAt)
	if !errors.Is(verifyErr, ErrModelVersionConflict) {
		t.Fatalf("VerifyAndUnblock error = %v, want model version conflict", verifyErr)
	}
}

func TestVerifyAndUnblockRejectsModelChangedDuringTest(t *testing.T) {
	service, db, _, discoverer := newCatalogTestService(t)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "verify-race", UpstreamID: "vendor/verify-race", DisplayName: "Verify race", Kind: KindChat, Enabled: true,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	modelID := modelByPublicID(t, service, "verify-race").ID
	keyID := insertNVIDIAKey(t, db)
	status := 403
	if err := service.BlockKeyModel(context.Background(), keyID, modelID, "model_forbidden", &status); err != nil {
		t.Fatalf("BlockKeyModel: %v", err)
	}
	discoverer.chatStarted = make(chan struct{})
	discoverer.chatRelease = make(chan struct{})
	discoverer.chatResponse = `{"choices":[{"message":{"content":"ok"}}]}`
	verifyErr := make(chan error, 1)
	go func() {
		_, verifyErrValue := service.VerifyAndUnblock(context.Background(), keyID, modelID)
		verifyErr <- verifyErrValue
	}()
	<-discoverer.chatStarted
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "verify-race", UpstreamID: "vendor/changed", DisplayName: "Verify race", Kind: KindChat, Enabled: true,
	}}); err != nil {
		t.Fatalf("change model during test: %v", err)
	}
	close(discoverer.chatRelease)
	if err := <-verifyErr; err == nil || !strings.Contains(err.Error(), "model version conflict") {
		t.Fatalf("VerifyAndUnblock error = %v, want model version conflict", err)
	}
	assertBlockCount(t, db, 1)
	var verifiedAt sql.NullString
	if err := db.QueryRow("SELECT capability_verified_at FROM models WHERE id = ?", modelID).Scan(&verifiedAt); err != nil {
		t.Fatalf("query capability verification: %v", err)
	}
	if verifiedAt.Valid {
		t.Fatalf("capability_verified_at = %q, want unset", verifiedAt.String)
	}
}

func TestResolveEnforcesKindAndCapabilities(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	if err := service.SaveSelection(context.Background(), []Selection{
		{
			PublicID: "capable", UpstreamID: "vendor/capable", DisplayName: "Capable", Kind: KindChat, Enabled: true,
			SupportsVision: true, SupportsTools: true, ToolsStatus: ToolsStatusSupported, SupportsReasoning: true, ReasoningWireFormat: "openai",
		},
		{PublicID: "plain", UpstreamID: "vendor/plain", DisplayName: "Plain", Kind: KindChat, Enabled: true},
		{PublicID: "embedding", UpstreamID: "vendor/embed", DisplayName: "Embedding", Kind: KindEmbedding, Enabled: true},
	}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}

	for _, requirements := range []Requirements{
		{Kind: KindChat, Vision: true},
		{Kind: KindChat, Tools: true},
		{Kind: KindChat, Reasoning: true},
	} {
		if _, err := service.Resolve(context.Background(), "capable", requirements); err != nil {
			t.Fatalf("Resolve capable %+v: %v", requirements, err)
		}
		if _, err := service.Resolve(context.Background(), "plain", requirements); !errors.Is(err, ErrCapabilityUnsupported) {
			t.Fatalf("Resolve plain %+v error = %v", requirements, err)
		}
	}
	if _, err := service.Resolve(context.Background(), "embedding", Requirements{Kind: KindChat}); !errors.Is(err, ErrModelKindMismatch) {
		t.Fatalf("kind mismatch error = %v", err)
	}
}

func TestResolveNamesUnsupportedCapability(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "plain", UpstreamID: "vendor/plain", DisplayName: "Plain", Kind: KindChat, Enabled: true,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}

	tests := []struct {
		name         string
		requirements Requirements
		capability   string
	}{
		{name: "vision", requirements: Requirements{Kind: KindChat, Vision: true}, capability: "vision"},
		{name: "reasoning", requirements: Requirements{Kind: KindChat, Reasoning: true}, capability: "reasoning"},
		{name: "tools", requirements: Requirements{Kind: KindChat, Tools: true}, capability: "tools"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := service.Resolve(context.Background(), "plain", testCase.requirements)
			if !errors.Is(err, ErrCapabilityUnsupported) {
				t.Fatalf("Resolve error = %v, want ErrCapabilityUnsupported", err)
			}
			if !strings.Contains(err.Error(), testCase.capability) {
				t.Fatalf("Resolve error = %q, want capability %q", err, testCase.capability)
			}
		})
	}
}

func TestResolveAdmitsInferredToolsCapability(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "inferred-tools", UpstreamID: "vendor/inferred-tools", DisplayName: "Inferred tools",
		Kind: KindChat, Enabled: true, SupportsTools: true,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	// inferred is an explicit claim (operator PATCH or capability hint), so a
	// tools request must route instead of deadlocking on 501 capability_unverified.
	if _, err := service.Resolve(context.Background(), "inferred-tools", Requirements{Kind: KindChat, Tools: true}); err != nil {
		t.Fatalf("Resolve inferred tools error = %v, want nil", err)
	}
	// A model whose tools support was never claimed stays blocked even when the
	// boolean flag is set: only supported/inferred describe verified or claimed.
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "unknown-tools", UpstreamID: "vendor/unknown-tools", DisplayName: "Unknown tools",
		Kind: KindChat, Enabled: true, SupportsTools: true, ToolsStatus: ToolsStatusUnknown,
	}}); err != nil {
		t.Fatalf("SaveSelection unknown-tools: %v", err)
	}
	if _, err := service.Resolve(context.Background(), "unknown-tools", Requirements{Kind: KindChat, Tools: true}); !errors.Is(err, ErrCapabilityUnverified) {
		t.Fatalf("Resolve unknown-tools error = %v, want ErrCapabilityUnverified", err)
	}
}

func TestAudioModelsRequireVerificationBeforeEnable(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	for _, kind := range []Kind{KindASR, KindTTS} {
		selection := Selection{PublicID: string(kind), UpstreamID: "vendor/" + string(kind), DisplayName: string(kind), Kind: kind, Enabled: true}
		if err := service.SaveSelection(context.Background(), []Selection{selection}); !errors.Is(err, ErrCapabilityUnverified) {
			t.Fatalf("SaveSelection(%s) error = %v", kind, err)
		}

		verifiedAt := time.Date(2026, 7, 30, 4, 0, 0, 0, time.UTC)
		selection.CapabilityVerifiedAt = &verifiedAt
		if err := service.SaveSelection(context.Background(), []Selection{selection}); err != nil {
			t.Fatalf("SaveSelection verified %s: %v", kind, err)
		}
		model, err := service.Resolve(context.Background(), string(kind), Requirements{Kind: kind})
		if err != nil {
			t.Fatalf("Resolve verified %s: %v", kind, err)
		}
		if err := service.SetCapabilityVerified(context.Background(), model.ID, nil); err != nil {
			t.Fatalf("SetCapabilityVerified nil: %v", err)
		}
		if err := service.SetEnabled(context.Background(), model.ID, true); !errors.Is(err, ErrCapabilityUnverified) {
			t.Fatalf("enable unverified %s error = %v", kind, err)
		}
	}
}

func TestRepositorySetEnabledRejectsUnverifiedAudioModel(t *testing.T) {
	_, db, _, _ := newCatalogTestService(t)
	repository := NewRepository(db)
	now := time.Date(2026, 7, 31, 4, 0, 0, 0, time.UTC)
	result, err := repository.SaveSelectionsResult(context.Background(), []Selection{{
		PublicID: "repository-unverified-asr", UpstreamID: "vendor/repository-unverified-asr",
		DisplayName: "Repository unverified ASR", Kind: KindASR, ReasoningWireFormat: "none",
	}}, now)
	if err != nil {
		t.Fatalf("SaveSelectionsResult: %v", err)
	}

	err = repository.SetEnabled(context.Background(), result.Models[0].ID, true, now)
	if !errors.Is(err, ErrCapabilityUnverified) {
		t.Fatalf("SetEnabled error = %v, want ErrCapabilityUnverified", err)
	}
}

func TestRepositorySetCapabilityVerifiedDisablesUnverifiedAudioModel(t *testing.T) {
	_, db, _, _ := newCatalogTestService(t)
	repository := NewRepository(db)
	now := time.Date(2026, 7, 31, 4, 0, 0, 0, time.UTC)
	verifiedAt := now.Add(-time.Hour)
	result, err := repository.SaveSelectionsResult(context.Background(), []Selection{{
		PublicID: "repository-verified-asr", UpstreamID: "vendor/repository-verified-asr",
		DisplayName: "Repository verified ASR", Kind: KindASR, Enabled: true, ReasoningWireFormat: "none",
		CapabilityVerifiedAt: &verifiedAt,
	}}, now)
	if err != nil {
		t.Fatalf("SaveSelectionsResult: %v", err)
	}

	if err := repository.SetCapabilityVerified(context.Background(), result.Models[0].ID, nil, now); err != nil {
		t.Fatalf("SetCapabilityVerified: %v", err)
	}
	var enabled int
	if err := db.QueryRow("SELECT enabled FROM models WHERE id = ?", result.Models[0].ID).Scan(&enabled); err != nil {
		t.Fatalf("load enabled state: %v", err)
	}
	if enabled != 0 {
		t.Fatalf("enabled = %d, want 0", enabled)
	}
}

func TestChangingModelKindClearsModelBlocks(t *testing.T) {
	service, db, _, _ := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)
	if err := service.SaveSelection(context.Background(), []Selection{{PublicID: "kind-block", UpstreamID: "vendor/kind-block", DisplayName: "Kind Block", Kind: KindChat, Enabled: true}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	model, err := service.Resolve(context.Background(), "kind-block", Requirements{Kind: KindChat})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	status := 403
	if err := service.BlockKeyModel(context.Background(), keyID, model.ID, "model_forbidden", &status); err != nil {
		t.Fatalf("BlockKeyModel: %v", err)
	}
	if _, err := service.Patch(context.Background(), model.ID, Patch{Kind: kindPointer(KindEmbedding)}); err != nil {
		t.Fatalf("Patch kind: %v", err)
	}
	assertBlockCount(t, db, 0)
}

func TestChangingModelKindClearsCapabilityVerification(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	verifiedAt := time.Date(2026, 7, 30, 4, 0, 0, 0, time.UTC)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "speech", UpstreamID: "vendor/speech", DisplayName: "Speech",
		Kind: KindASR, CapabilityVerifiedAt: &verifiedAt,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	model := modelByPublicID(t, service, "speech")

	chat, err := service.Patch(context.Background(), model.ID, Patch{Kind: kindPointer(KindChat)})
	if err != nil {
		t.Fatalf("Patch to chat: %v", err)
	}
	if chat.CapabilityVerifiedAt != nil {
		t.Fatalf("chat retained audio verification: %v", chat.CapabilityVerifiedAt)
	}
	asr, err := service.Patch(context.Background(), model.ID, Patch{Kind: kindPointer(KindASR)})
	if err != nil {
		t.Fatalf("Patch back to asr: %v", err)
	}
	if asr.CapabilityVerifiedAt != nil {
		t.Fatalf("asr regained stale verification: %v", asr.CapabilityVerifiedAt)
	}
	if err := service.SetEnabled(context.Background(), model.ID, true); !errors.Is(err, ErrCapabilityUnverified) {
		t.Fatalf("enable stale-verified asr error = %v", err)
	}
}

func kindPointer(kind Kind) *Kind { return &kind }

func TestManualUnblockUsesTargetChatEndpoint(t *testing.T) {
	service, db, _, discoverer := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)
	if err := service.SaveSelection(context.Background(), []Selection{{PublicID: "manual-chat", UpstreamID: "vendor/manual-chat", DisplayName: "Manual Chat", Kind: KindChat, Enabled: true}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	model, err := service.Resolve(context.Background(), "manual-chat", Requirements{Kind: KindChat})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	status := 403
	if err := service.BlockKeyModel(context.Background(), keyID, model.ID, "model_forbidden", &status); err != nil {
		t.Fatalf("BlockKeyModel: %v", err)
	}
	discoverer.chatResponse = `{"choices":[{"message":{"content":"ok"}}]}`
	if _, err := service.VerifyAndUnblock(context.Background(), keyID, model.ID); err != nil {
		t.Fatalf("VerifyAndUnblock: %v", err)
	}
	if discoverer.chatCalls != 1 || discoverer.modelsCalls != 0 {
		t.Fatalf("target calls chat/models = %d/%d, want 1/0", discoverer.chatCalls, discoverer.modelsCalls)
	}
	assertBlockCount(t, db, 0)
}

func TestModelVerificationTimeoutUsesModelFirstTokenWindow(t *testing.T) {
	defaultTimeout := modelVerificationTimeoutFor(Model{})
	if defaultTimeout != 30*time.Second {
		t.Fatalf("default verification timeout = %s, want 30s", defaultTimeout)
	}

	slowWindow := 120000
	if got := modelVerificationTimeoutFor(Model{StreamFirstTokenTimeoutMS: &slowWindow}); got != 120*time.Second {
		t.Fatalf("slow-model verification timeout = %s, want 120s", got)
	}

	tooLarge := 30 * 60 * 1000
	if got := modelVerificationTimeoutFor(Model{StreamFirstTokenTimeoutMS: &tooLarge}); got != maxModelVerificationTimeout {
		t.Fatalf("verification timeout cap = %s, want %s", got, maxModelVerificationTimeout)
	}
}

func TestChatModelProbeUsesValidMinimumOutputBudget(t *testing.T) {
	service, db, _, discoverer := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "probe-budget", UpstreamID: "vendor/probe-budget", DisplayName: "Probe Budget", Kind: KindChat,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	modelID := modelIDByPublicID(t, db, "probe-budget")
	discoverer.chatResponse = `{"choices":[{"message":{"content":"ok"}}]}`

	if err := service.TestModel(context.Background(), ProviderNVIDIA, keyID, modelID); err != nil {
		t.Fatalf("TestModel: %v", err)
	}
	if len(discoverer.chatBodies) != 1 {
		t.Fatalf("chat probe calls = %d, want 1", len(discoverer.chatBodies))
	}
	var payload map[string]any
	if err := json.Unmarshal(discoverer.chatBodies[0], &payload); err != nil {
		t.Fatalf("decode chat probe: %v", err)
	}
	if got := payload["max_tokens"]; got != float64(modelProbeMaxTokens) {
		t.Fatalf("chat probe max_tokens = %v, want %d", got, modelProbeMaxTokens)
	}
}

func TestChatModelProbePassesModelVerificationWindowToTransport(t *testing.T) {
	service, db, _, discoverer := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "probe-timeout", UpstreamID: "vendor/probe-timeout", DisplayName: "Probe Timeout", Kind: KindChat,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	modelID := modelIDByPublicID(t, db, "probe-timeout")
	slowWindow := 300000
	if _, err := service.Patch(context.Background(), modelID, Patch{StreamFirstTokenTimeoutMS: &slowWindow}); err != nil {
		t.Fatalf("Patch timeout: %v", err)
	}
	discoverer.chatResponse = `{"choices":[{"message":{"content":"ok"}}]}`

	if err := service.TestModel(context.Background(), ProviderNVIDIA, keyID, modelID); err != nil {
		t.Fatalf("TestModel: %v", err)
	}
	if len(discoverer.chatSnapshots) != 1 {
		t.Fatalf("chat probe snapshots = %d, want 1", len(discoverer.chatSnapshots))
	}
	if got := discoverer.chatSnapshots[0].FirstByteTimeoutMS; got != slowWindow {
		t.Fatalf("chat probe first-byte timeout = %d, want %d", got, slowWindow)
	}
}

func TestVerifyAndUnblockAudioModelsMarksVerificationAndClearsBlock(t *testing.T) {
	service, db, _, discoverer := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)
	for _, kind := range []Kind{KindASR, KindTTS} {
		if err := service.SaveSelection(context.Background(), []Selection{{PublicID: string(kind), UpstreamID: "vendor/" + string(kind), DisplayName: string(kind), Kind: kind}}); err != nil {
			t.Fatalf("save %s: %v", kind, err)
		}
		model, err := service.repository.Get(context.Background(), modelIDByPublicID(t, db, string(kind)))
		if err != nil {
			t.Fatalf("get %s: %v", kind, err)
		}
		status := 403
		if err := service.BlockKeyModel(context.Background(), keyID, model.ID, "model_forbidden", &status); err != nil {
			t.Fatalf("block %s: %v", kind, err)
		}
		verified, err := service.VerifyAndUnblock(context.Background(), keyID, model.ID)
		if err != nil {
			t.Fatalf("verify %s: %v", kind, err)
		}
		if verified.CapabilityVerifiedAt == nil {
			t.Fatalf("%s missing capability verification", kind)
		}
		stored, err := service.repository.Get(context.Background(), model.ID)
		if err != nil || stored.CapabilityVerifiedAt == nil {
			t.Fatalf("stored %s verification = %v/%v", kind, stored.CapabilityVerifiedAt, err)
		}
		assertBlockCount(t, db, 0)
		if kind == KindASR && discoverer.asrCalls != 1 {
			t.Fatalf("ASR calls = %d", discoverer.asrCalls)
		}
		if kind == KindTTS && discoverer.ttsCalls != 1 {
			t.Fatalf("TTS calls = %d", discoverer.ttsCalls)
		}
	}
}

func TestVerifyAndUnblockAudioFailureDoesNotWriteVerification(t *testing.T) {
	service, db, _, discoverer := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)
	for _, kind := range []Kind{KindASR, KindTTS} {
		publicID := "failed-" + string(kind)
		if err := service.SaveSelection(context.Background(), []Selection{{PublicID: publicID, UpstreamID: "vendor/" + string(kind), DisplayName: string(kind), Kind: kind}}); err != nil {
			t.Fatalf("save %s: %v", kind, err)
		}
		modelID := modelIDByPublicID(t, db, publicID)
		status := 403
		if err := service.BlockKeyModel(context.Background(), keyID, modelID, "model_forbidden", &status); err != nil {
			t.Fatalf("block %s: %v", kind, err)
		}
		if kind == KindASR {
			discoverer.asrResponse = `{"error":"bad"}`
		} else {
			discoverer.ttsResponse = ""
			discoverer.ttsResponseSet = true
		}
		if _, err := service.VerifyAndUnblock(context.Background(), keyID, modelID); !errors.Is(err, ErrManualTestRequired) {
			t.Fatalf("verify %s error = %v", kind, err)
		}
		stored, err := service.repository.Get(context.Background(), modelID)
		if err != nil {
			t.Fatalf("get failed %s: %v", kind, err)
		}
		if stored.CapabilityVerifiedAt != nil {
			t.Fatalf("failed %s wrote verification %v", kind, stored.CapabilityVerifiedAt)
		}
		assertBlockCount(t, db, 1)
		if err := service.UnblockKeyModel(context.Background(), keyID, modelID, true); err != nil {
			t.Fatalf("cleanup %s: %v", kind, err)
		}
	}
}

func TestVerifyAndUnblockProxyErrorWritesNothing(t *testing.T) {
	service, db, _, discoverer := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "proxy-block", UpstreamID: "vendor/proxy-block", DisplayName: "Proxy Block", Kind: KindChat, Enabled: true,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	modelID := modelIDByPublicID(t, db, "proxy-block")
	status := 403
	if err := service.BlockKeyModel(context.Background(), keyID, modelID, "model_forbidden", &status); err != nil {
		t.Fatalf("BlockKeyModel: %v", err)
	}
	discoverer.chatErr = xkproxy.NewTransportError(errors.New("private proxy cause"))

	_, err := service.VerifyAndUnblock(context.Background(), keyID, modelID)
	var proxyErr *xkproxy.Error
	if !errors.As(err, &proxyErr) {
		t.Fatalf("VerifyAndUnblock error = %T %v, want *xkproxy.Error", err, err)
	}
	if errors.Is(err, ErrManualTestRequired) {
		t.Fatalf("VerifyAndUnblock mapped proxy error to manual test required: %v", err)
	}
	if discoverer.chatCalls != 1 {
		t.Fatalf("chat calls = %d, want 1", discoverer.chatCalls)
	}
	// The proxy failure must leave every verification artifact untouched:
	// capability_verified_at stays unset, the key-model block survives, and no
	// NVIDIA key state is written.
	stored, err := service.repository.Get(context.Background(), modelID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.CapabilityVerifiedAt != nil {
		t.Fatalf("proxy error wrote capability_verified_at = %v", stored.CapabilityVerifiedAt)
	}
	assertBlock(t, db, keyID, modelID, "model_forbidden", 403)
	assertBlockCount(t, db, 1)
	var authInvalid, consecutiveFailures, cooldownLevel int
	var lastErrorCode sql.NullString
	if err := db.QueryRow(`
		SELECT auth_invalid, consecutive_failures, cooldown_level, last_error_code
		FROM nvidia_keys WHERE id = ?
	`, keyID).Scan(&authInvalid, &consecutiveFailures, &cooldownLevel, &lastErrorCode); err != nil {
		t.Fatalf("query NVIDIA key state: %v", err)
	}
	if authInvalid != 0 || consecutiveFailures != 0 || cooldownLevel != 0 || lastErrorCode.Valid {
		t.Fatalf("NVIDIA key state mutated by proxy error = invalid:%d failures:%d cooldown:%d error:%v",
			authInvalid, consecutiveFailures, cooldownLevel, lastErrorCode)
	}
}

func TestAudioVerificationRejectsMissingOrNonAudioContentType(t *testing.T) {
	cases := []struct {
		name              string
		contentType       string
		contentTypeHeader bool
		body              string
	}{
		{name: "missing-header", body: "audio"},
		{name: "empty-header", contentTypeHeader: true, body: "audio"},
		{name: "empty-body", contentType: "audio/wav", contentTypeHeader: true},
		{name: "text", contentType: "text/plain", contentTypeHeader: true, body: "audio"},
		{name: "json", contentType: "application/json", contentTypeHeader: true, body: "audio"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service, db, _, discoverer := newCatalogTestService(t)
			keyID := insertNVIDIAKey(t, db)
			if err := service.SaveSelection(context.Background(), []Selection{{
				PublicID: "tts-" + testCase.name, UpstreamID: "vendor/tts", DisplayName: "TTS", Kind: KindTTS,
			}}); err != nil {
				t.Fatalf("SaveSelection: %v", err)
			}
			modelID := modelIDByPublicID(t, db, "tts-"+testCase.name)
			status := 403
			if err := service.BlockKeyModel(context.Background(), keyID, modelID, "model_forbidden", &status); err != nil {
				t.Fatalf("BlockKeyModel: %v", err)
			}
			discoverer.ttsContentType = testCase.contentType
			discoverer.ttsContentTypeSet = true
			discoverer.ttsContentTypeHeaderSet = testCase.contentTypeHeader
			discoverer.ttsResponse = testCase.body
			discoverer.ttsResponseSet = true

			if _, err := service.VerifyAndUnblock(context.Background(), keyID, modelID); !errors.Is(err, ErrManualTestRequired) {
				t.Fatalf("VerifyAndUnblock error = %v, want manual test required", err)
			}
			stored, err := service.repository.Get(context.Background(), modelID)
			if err != nil {
				t.Fatalf("get failed model: %v", err)
			}
			if stored.CapabilityVerifiedAt != nil {
				t.Fatalf("failed verification wrote timestamp %v", stored.CapabilityVerifiedAt)
			}
			assertBlockCount(t, db, 1)
		})
	}
}

func TestAudioVerificationAcceptsAnyAudioContentType(t *testing.T) {
	for _, contentType := range []string{"audio/wav", "audio/m4a; charset=binary", "audio/*", "application/octet-stream"} {
		t.Run(contentType, func(t *testing.T) {
			service, db, _, discoverer := newCatalogTestService(t)
			keyID := insertNVIDIAKey(t, db)
			if err := service.SaveSelection(context.Background(), []Selection{{
				PublicID: "tts", UpstreamID: "vendor/tts", DisplayName: "TTS", Kind: KindTTS,
			}}); err != nil {
				t.Fatalf("SaveSelection: %v", err)
			}
			modelID := modelIDByPublicID(t, db, "tts")
			status := 403
			if err := service.BlockKeyModel(context.Background(), keyID, modelID, "model_forbidden", &status); err != nil {
				t.Fatalf("BlockKeyModel: %v", err)
			}
			discoverer.ttsContentType = contentType
			discoverer.ttsContentTypeSet = true
			discoverer.ttsContentTypeHeaderSet = true
			discoverer.ttsResponse = "audio"
			discoverer.ttsResponseSet = true

			verified, err := service.VerifyAndUnblock(context.Background(), keyID, modelID)
			if err != nil {
				t.Fatalf("VerifyAndUnblock: %v", err)
			}
			if verified.CapabilityVerifiedAt == nil {
				t.Fatal("successful verification did not set timestamp")
			}
			assertBlockCount(t, db, 0)
		})
	}
}

func modelIDByPublicID(t *testing.T, db *sql.DB, publicID string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow("SELECT id FROM models WHERE public_id = ?", publicID).Scan(&id); err != nil {
		t.Fatalf("model ID %s: %v", publicID, err)
	}
	return id
}

func TestModelsListingCannotVerifyAudioEndpointCapability(t *testing.T) {
	service, db, _, discoverer := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)

	for _, kind := range []Kind{KindASR, KindTTS} {
		upstreamID := "vendor/" + string(kind)
		if err := service.SaveSelection(context.Background(), []Selection{{
			PublicID: string(kind), UpstreamID: upstreamID, DisplayName: string(kind), Kind: kind,
		}}); err != nil {
			t.Fatalf("SaveSelection(%s): %v", kind, err)
		}
		models, err := service.List(context.Background())
		if err != nil {
			t.Fatalf("List(%s): %v", kind, err)
		}
		var model Model
		for _, item := range models {
			if item.PublicID == string(kind) {
				model = item
				break
			}
		}
		status := 403
		if err := service.BlockKeyModel(context.Background(), keyID, model.ID, "model_forbidden", &status); err != nil {
			t.Fatalf("BlockKeyModel(%s): %v", kind, err)
		}
		discoverer.models = []string{upstreamID}
		if _, err := service.DiscoverCandidates(context.Background(), keyID); err != nil {
			t.Fatalf("DiscoverCandidates(%s): %v", kind, err)
		}
		// Discovery alone is not the audit verification path. A successful
		// endpoint probe is required before capability_verified_at is written.
		stored, err := service.repository.Get(context.Background(), model.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", kind, err)
		}
		if stored.CapabilityVerifiedAt != nil {
			t.Fatalf("%s capability verified by /models discovery: %v", kind, stored.CapabilityVerifiedAt)
		}
		assertBlockCount(t, db, 1)

		if err := service.UnblockKeyModel(context.Background(), keyID, model.ID, true); err != nil {
			t.Fatalf("cleanup block(%s): %v", kind, err)
		}
	}
}

func TestBlockUpsertsAndOnlySuccessfulManualTestCanUnblock(t *testing.T) {
	service, db, _, _ := newCatalogTestService(t)
	keyID := insertNVIDIAKey(t, db)
	if err := service.SaveSelection(context.Background(), []Selection{
		{PublicID: "blocked", UpstreamID: "vendor/blocked", DisplayName: "Blocked", Kind: KindChat, Enabled: true},
	}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	model, err := service.Resolve(context.Background(), "blocked", Requirements{Kind: KindChat})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	status403 := 403
	if err := service.BlockKeyModel(context.Background(), keyID, model.ID, "model_forbidden", &status403); err != nil {
		t.Fatalf("BlockKeyModel: %v", err)
	}
	status404 := 404
	if err := service.BlockKeyModel(context.Background(), keyID, model.ID, "model_not_found", &status404); err != nil {
		t.Fatalf("BlockKeyModel upsert: %v", err)
	}
	assertBlock(t, db, keyID, model.ID, "model_not_found", 404)

	if err := service.UnblockKeyModel(context.Background(), keyID, model.ID, false); !errors.Is(err, ErrManualTestRequired) {
		t.Fatalf("Unblock without successful test error = %v", err)
	}
	assertBlockCount(t, db, 1)
	if err := service.UnblockKeyModel(context.Background(), keyID, model.ID, true); err != nil {
		t.Fatalf("Unblock after successful test: %v", err)
	}
	assertBlockCount(t, db, 0)

	if err := service.BlockKeyModel(context.Background(), keyID, model.ID, "model_forbidden", &status403); err != nil {
		t.Fatalf("BlockKeyModel before delete: %v", err)
	}
	if err := service.DeleteModel(context.Background(), model.ID); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	assertBlockCount(t, db, 0)

	if err := service.SaveSelection(context.Background(), []Selection{
		{PublicID: "cascade-key", UpstreamID: "vendor/cascade", DisplayName: "Cascade", Kind: KindChat, Enabled: true},
	}); err != nil {
		t.Fatalf("SaveSelection cascade key: %v", err)
	}
	cascadeModel, err := service.Resolve(context.Background(), "cascade-key", Requirements{Kind: KindChat})
	if err != nil {
		t.Fatalf("Resolve cascade key model: %v", err)
	}
	if err := service.BlockKeyModel(context.Background(), keyID, cascadeModel.ID, "model_forbidden", &status403); err != nil {
		t.Fatalf("BlockKeyModel before key delete: %v", err)
	}
	if _, err := db.Exec("DELETE FROM nvidia_keys WHERE id = ?", keyID); err != nil {
		t.Fatalf("delete NVIDIA key: %v", err)
	}
	assertBlockCount(t, db, 0)
}

func newCatalogTestService(t *testing.T) (*Service, *sql.DB, *fakeSecrets, *fakeDiscoverer) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	secrets := &fakeSecrets{}
	discoverer := &fakeDiscoverer{}
	return NewService(NewRepository(db), secrets, discoverer, nvidia.DefaultDescriptor(), catalogClock{}), db, secrets, discoverer
}

type fakeSecrets struct {
	lastKeyID    int64
	usedKeyIDs   []int64
	availableIDs []int64
	availableErr error
}

func (s *fakeSecrets) WithSecret(_ context.Context, keyID int64, callback func([]byte) error) error {
	s.lastKeyID = keyID
	s.usedKeyIDs = append(s.usedKeyIDs, keyID)
	secret := []byte("secret-for-" + strconv.FormatInt(keyID, 10))
	return callback(secret)
}

func (s *fakeSecrets) AvailableIDsShuffled(context.Context) ([]int64, error) {
	if s.availableErr != nil {
		return nil, s.availableErr
	}
	return append([]int64(nil), s.availableIDs...), nil
}

type fakeDiscoverer struct {
	models        []string
	modelsErr     error
	lastToken     string
	modelsCalls   int
	chatCalls     int
	chatBodies    [][]byte
	chatSnapshots []runtimeconfig.Snapshot
	chatResponse  string
	chatErr       error
	// chatStatuses replays one HTTP status per call so a test can make an early
	// probe attempt fail and a later one succeed.
	chatStatuses            []int
	asrCalls                int
	ttsCalls                int
	asrResponse             string
	ttsResponse             string
	ttsResponseSet          bool
	ttsContentType          string
	ttsContentTypeSet       bool
	ttsContentTypeHeaderSet bool
	chatStarted             chan struct{}
	chatRelease             chan struct{}
	// chatCompletion, when set, wraps the probe body so a test can observe
	// whether the probe declared semantic completion before closing it.
	chatCompletion *probeCompletion
	// chatResponses replays one body per Chat call, overriding chatResponse and
	// chatStatuses. A call past the slice falls back to the normal replay.
	chatResponses []string
}

func (d *fakeDiscoverer) Models(_ context.Context, token string) ([]string, error) {
	d.lastToken = token
	d.modelsCalls++
	if d.modelsErr != nil {
		return nil, d.modelsErr
	}
	return append([]string(nil), d.models...), nil
}

func (d *fakeDiscoverer) Chat(_ context.Context, snapshot runtimeconfig.Snapshot, _ string, probeBody []byte, _ bool) (*http.Response, error) {
	d.chatCalls++
	d.chatBodies = append(d.chatBodies, append([]byte(nil), probeBody...))
	d.chatSnapshots = append(d.chatSnapshots, snapshot)
	if d.chatErr != nil {
		return nil, d.chatErr
	}
	if d.chatStarted != nil {
		close(d.chatStarted)
		<-d.chatRelease
	}
	body := d.chatResponse
	if body == "" {
		body = `{"choices":[]}`
	}
	if len(d.chatResponses) > 0 {
		body = d.chatResponses[0]
		d.chatResponses = d.chatResponses[1:]
	}
	status := http.StatusOK
	if len(d.chatStatuses) > 0 {
		status = d.chatStatuses[0]
		d.chatStatuses = d.chatStatuses[1:]
	}
	reader := io.NopCloser(strings.NewReader(body))
	if d.chatCompletion != nil {
		reader = probeCompletionBody{ReadCloser: reader, tracker: d.chatCompletion}
	}
	return &http.Response{StatusCode: status, Body: reader, Header: make(http.Header)}, nil
}

func (d *fakeDiscoverer) AudioTranscriptions(_ context.Context, _ runtimeconfig.Snapshot, _ string, body []byte, contentType string) (*http.Response, error) {
	d.asrCalls++
	if !strings.Contains(contentType, "multipart/form-data") || !bytes.Contains(body, []byte("vendor/asr")) || !bytes.Contains(body, []byte("RIFF")) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":"invalid probe"}`)), Header: make(http.Header)}, nil
	}
	response := d.asrResponse
	if response == "" {
		response = `{"text":"ok"}`
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
}

func (d *fakeDiscoverer) AudioSpeech(_ context.Context, _ runtimeconfig.Snapshot, _ string, body []byte) (*http.Response, error) {
	d.ttsCalls++
	if !bytes.Contains(body, []byte("vendor/tts")) || !bytes.Contains(body, []byte("Magpie-Multilingual.EN-US.Aria")) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":"invalid probe"}`)), Header: make(http.Header)}, nil
	}
	response := d.ttsResponse
	if !d.ttsResponseSet {
		response = "audio"
	}
	contentType := d.ttsContentType
	header := make(http.Header)
	if !d.ttsContentTypeSet {
		contentType = "audio/wav"
		header.Set("Content-Type", contentType)
	} else if d.ttsContentTypeHeaderSet {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: header}, nil
}

type catalogClock struct{}

type fakeOpenCodeFreeGateway struct {
	models     []string
	modelsErr  error
	chatCalls  int
	chatBodies [][]byte
	// chatResponse, when set, makes Chat answer 200 with that body so the
	// candidate-probe path can be exercised; empty keeps Chat refusing.
	chatResponse string
}

func (f *fakeOpenCodeFreeGateway) Models(context.Context) ([]string, error) {
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return append([]string(nil), f.models...), nil
}

func (f *fakeOpenCodeFreeGateway) Chat(_ context.Context, _ runtimeconfig.Snapshot, probeBody []byte, _ bool) (*http.Response, error) {
	f.chatCalls++
	f.chatBodies = append(f.chatBodies, append([]byte(nil), probeBody...))
	if f.chatResponse == "" {
		return nil, errors.New("discovery must not call the gateway chat endpoint")
	}
	reader := io.NopCloser(strings.NewReader(f.chatResponse))
	return &http.Response{StatusCode: http.StatusOK, Body: reader, Header: make(http.Header)}, nil
}

func (catalogClock) Now() time.Time                              { return time.Date(2026, 7, 30, 4, 0, 0, 0, time.UTC) }
func (catalogClock) NewTimer(duration time.Duration) *time.Timer { return time.NewTimer(duration) }
func (catalogClock) AfterFunc(duration time.Duration, callback func()) *time.Timer {
	return time.AfterFunc(duration, callback)
}

func candidateIDs(candidates []Candidate) []string {
	ids := make([]string, len(candidates))
	for index, candidate := range candidates {
		ids[index] = candidate.UpstreamID
	}
	return ids
}

func assertModelCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM models").Scan(&count); err != nil {
		t.Fatalf("count models: %v", err)
	}
	if count != want {
		t.Fatalf("model count = %d, want %d", count, want)
	}
}

// modelByPublicID returns the model with the given public ID. Tests that create a
// selection and then read it back used models[0], which the DeepSeek seed migration
// (015) shifted because it inserts a row earlier than the test's selection.
func modelByPublicID(t *testing.T, service *Service, publicID string) Model {
	t.Helper()
	models, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, model := range models {
		if model.PublicID == publicID {
			return model
		}
	}
	t.Fatalf("model %q not found in %v", publicID, models)
	return Model{}
}

func insertNVIDIAKey(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO nvidia_keys (
			ciphertext, nonce, fingerprint, display_prefix, display_suffix, created_at, updated_at
		) VALUES (x'01', x'02', x'03', 'prefix', 'suffix', '2026-07-30T04:00:00Z', '2026-07-30T04:00:00Z')
	`)
	if err != nil {
		t.Fatalf("insert NVIDIA key: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("NVIDIA key ID: %v", err)
	}
	return id
}

func assertBlock(t *testing.T, db *sql.DB, keyID, modelID int64, wantReason string, wantStatus int) {
	t.Helper()
	var reason string
	var status int
	if err := db.QueryRow(`
		SELECT reason_code, upstream_status
		FROM nvidia_key_model_blocks
		WHERE nvidia_key_id = ? AND model_id = ?
	`, keyID, modelID).Scan(&reason, &status); err != nil {
		t.Fatalf("query block: %v", err)
	}
	if reason != wantReason || status != wantStatus {
		t.Fatalf("block = %q/%d, want %q/%d", reason, status, wantReason, wantStatus)
	}
}

func assertBlockCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM nvidia_key_model_blocks").Scan(&count); err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if count != want {
		t.Fatalf("block count = %d, want %d", count, want)
	}
}

func TestListIncludesBlockedNVIDIAKeyIDs(t *testing.T) {
	service, db, _, _ := newCatalogTestService(t)
	firstKeyID := insertNVIDIAKey(t, db)
	secondResult, err := db.Exec(`
		INSERT INTO nvidia_keys (
			ciphertext, nonce, fingerprint, display_prefix, display_suffix, created_at, updated_at
		) VALUES (x'04', x'05', x'06', 'prefix2', 'suffix2', '2026-07-30T04:00:00Z', '2026-07-30T04:00:00Z')
	`)
	if err != nil {
		t.Fatalf("insert second NVIDIA key: %v", err)
	}
	secondKeyID, err := secondResult.LastInsertId()
	if err != nil {
		t.Fatalf("second NVIDIA key ID: %v", err)
	}
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "blocked-list", UpstreamID: "vendor/blocked-list", DisplayName: "Blocked List", Kind: KindChat, Enabled: true,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	modelID := modelByPublicID(t, service, "blocked-list").ID
	status := 403
	if err := service.BlockKeyModel(context.Background(), secondKeyID, modelID, "model_forbidden", &status); err != nil {
		t.Fatalf("Block second key: %v", err)
	}
	if err := service.BlockKeyModel(context.Background(), firstKeyID, modelID, "model_forbidden", &status); err != nil {
		t.Fatalf("Block first key: %v", err)
	}

	if got := modelByPublicID(t, service, "blocked-list").BlockedByKeyIDs; !reflect.DeepEqual(got, []int64{firstKeyID, secondKeyID}) {
		t.Fatalf("BlockedByKeyIDs = %v, want [%d %d]", got, firstKeyID, secondKeyID)
	}
}

// TestPatchStreamTimeoutsPersistsAndRoundsTrips proves the per-model streaming
// timeout override columns are patchable and round-trip through the repository
// (migration 016/022 seeded them, but the Patch surface previously dropped them).
func TestPatchStreamTimeoutsPersistsAndRoundsTrips(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	result, err := service.SaveSelectionResult(context.Background(), []Selection{{
		PublicID: "stream-timeout", UpstreamID: "vendor/stream-timeout", DisplayName: "Stream timeout", Kind: KindChat,
	}})
	if err != nil {
		t.Fatalf("SaveSelectionResult: %v", err)
	}
	modelID := result.Models[0].ID

	firstToken := 240000
	idle := 300000
	patched, err := service.Patch(context.Background(), modelID, Patch{
		StreamFirstTokenTimeoutMS: &firstToken,
		StreamIdleTimeoutMS:       &idle,
	})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if patched.StreamFirstTokenTimeoutMS == nil || *patched.StreamFirstTokenTimeoutMS != firstToken {
		t.Fatalf("StreamFirstTokenTimeoutMS = %v, want %d", patched.StreamFirstTokenTimeoutMS, firstToken)
	}
	if patched.StreamIdleTimeoutMS == nil || *patched.StreamIdleTimeoutMS != idle {
		t.Fatalf("StreamIdleTimeoutMS = %v, want %d", patched.StreamIdleTimeoutMS, idle)
	}

	// A second patch omitting the timeout fields must preserve the stored values.
	display := "Renamed"
	again, err := service.Patch(context.Background(), modelID, Patch{DisplayName: &display})
	if err != nil {
		t.Fatalf("second Patch: %v", err)
	}
	if again.StreamFirstTokenTimeoutMS == nil || *again.StreamFirstTokenTimeoutMS != firstToken {
		t.Fatalf("StreamFirstTokenTimeoutMS after display-only patch = %v, want preserved %d", again.StreamFirstTokenTimeoutMS, firstToken)
	}
}

func TestPatchContextLengthPersistsAndRoundTrips(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	result, err := service.SaveSelectionResult(context.Background(), []Selection{{
		PublicID: "context-length", UpstreamID: "vendor/context-length", DisplayName: "Context length", Kind: KindChat,
	}})
	if err != nil {
		t.Fatalf("SaveSelectionResult: %v", err)
	}
	modelID := result.Models[0].ID

	length := 131072
	patched, err := service.Patch(context.Background(), modelID, Patch{ContextLength: &length})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if patched.ContextLength != length {
		t.Fatalf("ContextLength = %d, want %d", patched.ContextLength, length)
	}

	// A display-only patch must preserve the declaration, and re-saving the
	// discovery selection must not reset it either (context is operator-owned).
	display := "Renamed"
	if again, err := service.Patch(context.Background(), modelID, Patch{DisplayName: &display}); err != nil {
		t.Fatalf("second Patch: %v", err)
	} else if again.ContextLength != length {
		t.Fatalf("ContextLength after display-only patch = %d, want preserved %d", again.ContextLength, length)
	}
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "context-length", UpstreamID: "vendor/context-length", DisplayName: display, Kind: KindChat,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	model := modelByPublicID(t, service, "context-length")
	if model.ContextLength != length {
		t.Fatalf("ContextLength after selection re-save = %d, want preserved %d", model.ContextLength, length)
	}

	cleared := 0
	if patched, err := service.Patch(context.Background(), modelID, Patch{ContextLength: &cleared}); err != nil {
		t.Fatalf("clear Patch: %v", err)
	} else if patched.ContextLength != 0 {
		t.Fatalf("ContextLength after clear = %d, want 0", patched.ContextLength)
	}
}

// Re-validating the STORED provider wrapped validateEnabledProvider's result with
// %w unconditionally, and %w on a nil error still yields a non-nil error. Since
// that function accepts both providers, re-saving an existing OpenCodeFree model
// as enabled always failed — and SaveSelectionsResult runs one transaction, so
// unrelated models in the same batch were rolled back with it.
func TestSaveSelectionsReenablesStoredOpenCodeFreeModelWithBatchPeer(t *testing.T) {
	_, db, _, _ := newCatalogTestService(t)
	repository := NewRepository(db)
	now := time.Date(2026, 8, 21, 4, 0, 0, 0, time.UTC)
	gateway := Selection{
		PublicID: "opencode-free/batch-probe", UpstreamID: "batch-probe",
		DisplayName: "Batch probe", Kind: KindChat, Provider: ProviderOpenCodeFree,
		Enabled: true, ReasoningWireFormat: "none",
	}
	if _, err := repository.SaveSelectionsResult(context.Background(), []Selection{gateway}, now); err != nil {
		t.Fatalf("seed OpenCodeFree model: %v", err)
	}

	peer := Selection{
		PublicID: "vendor/batch-peer", UpstreamID: "vendor/batch-peer",
		DisplayName: "Batch peer", Kind: KindChat, Provider: ProviderNVIDIA,
		Enabled: true, ReasoningWireFormat: "none",
	}
	if _, err := repository.SaveSelectionsResult(context.Background(), []Selection{gateway, peer}, now); err != nil {
		t.Fatalf("re-save stored OpenCodeFree model: %v", err)
	}

	// The peer proves the failure took the whole transaction down, not just its
	// own row.
	for _, publicID := range []string{gateway.PublicID, peer.PublicID} {
		var enabled int
		if err := db.QueryRow("SELECT enabled FROM models WHERE public_id = ?", publicID).Scan(&enabled); err != nil {
			t.Fatalf("load %q: %v", publicID, err)
		}
		if enabled != 1 {
			t.Fatalf("model %q enabled = %d, want 1", publicID, enabled)
		}
	}
}
