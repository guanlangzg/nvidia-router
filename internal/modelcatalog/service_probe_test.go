package modelcatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestHasValidToolCallsRequiresFunctionAndJSONArguments(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{name: "valid", body: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"weather","arguments":"{\"city\":\"Hangzhou\"}"}}]}}]}`, want: true},
		{name: "missing function name", body: `{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`, want: false},
		{name: "invalid arguments", body: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"weather","arguments":"not-json"}}]}}]}`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := hasValidToolCalls([]byte(test.body)); got != test.want {
				t.Fatalf("hasValidToolCalls() = %v, want %v", got, test.want)
			}
		})
	}
}

// probeService prepares one chat model and returns everything a probe test needs.
func probeService(t *testing.T, publicID string, availableKeys ...int64) (*Service, *fakeSecrets, *fakeDiscoverer, int64) {
	t.Helper()
	service, db, secrets, discoverer := newCatalogTestService(t)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: publicID, UpstreamID: "vendor/" + publicID, DisplayName: publicID, Kind: KindChat, Enabled: true,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	secrets.availableIDs = availableKeys
	discoverer.chatResponse = `{"choices":[{"message":{"content":"ok"}}]}`
	return service, secrets, discoverer, modelIDByPublicID(t, db, publicID)
}

// The operator no longer picks a credential, so a probe that hits a bad key must
// move on to another one instead of reporting the model as broken.
func TestModelAutoRotatesNVIDIAKeysUntilOneAnswers(t *testing.T) {
	service, secrets, discoverer, modelID := probeService(t, "rotate-chat", 11, 12, 13)
	discoverer.chatStatuses = []int{503}

	if err := service.TestModelAuto(context.Background(), modelID); err != nil {
		t.Fatalf("TestModelAuto: %v", err)
	}
	if len(secrets.usedKeyIDs) != 2 {
		t.Fatalf("keys tried = %v, want two (one failure then a success)", secrets.usedKeyIDs)
	}
	if secrets.usedKeyIDs[0] == secrets.usedKeyIDs[1] {
		t.Fatalf("retry reused key %d instead of switching", secrets.usedKeyIDs[0])
	}
}

// A 404 describes the model, not the credential: another key would answer the
// same way, so burning the remaining keys would only waste upstream quota.
func TestModelAutoStopsOnAnAnswerThatDescribesTheModel(t *testing.T) {
	service, secrets, discoverer, modelID := probeService(t, "missing-chat", 11, 12, 13)
	discoverer.chatStatuses = []int{404}

	err := service.TestModelAuto(context.Background(), modelID)
	if !errors.Is(err, ErrManualTestRequired) || errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("error = %v, want a plain ErrManualTestRequired", err)
	}
	if len(secrets.usedKeyIDs) != 1 {
		t.Fatalf("keys tried = %v, want exactly one", secrets.usedKeyIDs)
	}
}

func TestModelAutoBoundsKeyRotationToProbeAttempts(t *testing.T) {
	service, secrets, discoverer, modelID := probeService(t, "dead-chat", 11, 12, 13, 14, 15)
	discoverer.chatStatuses = []int{503, 503, 503, 503, 503}

	if err := service.TestModelAuto(context.Background(), modelID); !errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("error = %v, want ErrUpstreamUnreachable", err)
	}
	if len(secrets.usedKeyIDs) != probeAttempts {
		t.Fatalf("keys tried = %v, want %d", secrets.usedKeyIDs, probeAttempts)
	}
}

func TestModelAutoReportsWhenNoNVIDIAKeyIsAvailable(t *testing.T) {
	service, _, _, modelID := probeService(t, "keyless-chat")

	if err := service.TestModelAuto(context.Background(), modelID); !errors.Is(err, ErrNVIDIAKeyRequired) {
		t.Fatalf("error = %v, want ErrNVIDIAKeyRequired", err)
	}
}

// A mixed selection dispatches on each model's own provider: an OpenCodeFree
// model must never consume an NVIDIA key.
func TestModelAutoDispatchesOnTheModelsOwnProvider(t *testing.T) {
	service, db, secrets, _ := newCatalogTestService(t)
	secrets.availableIDs = []int64{11}
	insertOpenCodeFreeModel(t, db, "opencodefree/model")

	err := service.TestModelAuto(context.Background(), modelIDByPublicID(t, db, "opencodefree/model"))
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("error = %v, want ErrProviderNotConfigured", err)
	}
	if len(secrets.usedKeyIDs) != 0 {
		t.Fatalf("OpenCodeFree probe consumed NVIDIA keys %v", secrets.usedKeyIDs)
	}
}

func insertOpenCodeFreeModel(t *testing.T, db *sql.DB, publicID string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO models (public_id, upstream_id, display_name, kind, provider, enabled, created_at, updated_at)
		VALUES (?, ?, ?, 'chat', ?, 1, '2026-07-30T04:00:00Z', '2026-07-30T04:00:00Z')
	`, publicID, "vendor/"+publicID, publicID, ProviderOpenCodeFree); err != nil {
		t.Fatalf("insert OpenCodeFree model: %v", err)
	}
}

// probeCompletion records whether the probe told the upstream body that the
// answer completed. Non-stream bodies defer their EOF verdict until a validator
// confirms the payload, so a probe that forgets this charges a request failure
// against the pooled exit that served a perfectly good 200.
type probeCompletion struct{ marked bool }

type probeCompletionBody struct {
	io.ReadCloser
	tracker *probeCompletion
}

func (b probeCompletionBody) MarkComplete() { b.tracker.marked = true }

func TestModelProbeDeclaresCompletionOnSuccess(t *testing.T) {
	service, _, discoverer, modelID := probeService(t, "complete-chat", 11)
	tracker := &probeCompletion{}
	discoverer.chatCompletion = tracker

	if err := service.TestModelAuto(context.Background(), modelID); err != nil {
		t.Fatalf("TestModelAuto: %v", err)
	}
	if !tracker.marked {
		t.Fatal("a validated probe must mark completion, otherwise the exit that served it is charged a failure")
	}
}

const toolsProbeCallBody = `{"choices":[{"message":{"tool_calls":[{"function":{"name":"weather","arguments":"{\"city\":\"Hangzhou\"}"}}]}}]}`

// The periodic probe runner probes only enabled chat models and writes the
// verdict back through the same applyProbe path as the admin job.
func TestCapabilityProbeRunnerProbesEnabledChatModelsOnly(t *testing.T) {
	service, db, secrets, discoverer := newCatalogTestService(t)
	secrets.availableIDs = []int64{11}
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "probe-enabled", UpstreamID: "vendor/pe", DisplayName: "PE", Kind: KindChat, Enabled: true,
	}}); err != nil {
		t.Fatalf("SaveSelection chat: %v", err)
	}
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "probe-embed", UpstreamID: "vendor/pemb", DisplayName: "PEMB", Kind: KindEmbedding, Enabled: true,
	}}); err != nil {
		t.Fatalf("SaveSelection embedding: %v", err)
	}
	discoverer.chatResponse = `{"choices":[{"message":{"content":"ok"}}]}`
	discoverer.chatResponses = []string{
		`{"choices":[{"message":{"content":"ok"}}]}`, // base
		`{"choices":[{"message":{"content":"ok"}}]}`, // reasoning
		toolsProbeCallBody,                           // required form hits
	}

	runner := NewCapabilityProbeRunner(service, discardLogger())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.runOnceForTest(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("probe cycle did not finish")
	}

	// The seeded deepseek-v4-flash alias row is also enabled+chat, so both chat
	// models probe serially. The first model consumes the replay queue: base,
	// reasoning, then the required form hitting tool_call → supported (3 calls).
	// The second model falls through to the plain 200-without-calls response:
	// base + reasoning + both silent tools forms → unsupported (4 calls).
	// The embedding model must be skipped entirely.
	if discoverer.chatCalls != 7 {
		t.Fatalf("chat calls = %d, want 7 (embedding model must be skipped)", discoverer.chatCalls)
	}
	var supportedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM models WHERE kind = 'chat' AND tools_status = ?`, ToolsStatusSupported).Scan(&supportedCount); err != nil {
		t.Fatalf("count supported models: %v", err)
	}
	if supportedCount != 1 {
		t.Fatalf("supported chat models = %d, want 1", supportedCount)
	}
}

// Candidates carry no whitelist row: the probe must work straight from the
// discovery payload and leave the catalog untouched.
func TestCandidateProbesWithoutAWhitelistRow(t *testing.T) {
	service, db, secrets, discoverer := newCatalogTestService(t)
	secrets.availableIDs = []int64{11}
	discoverer.chatResponse = `{"choices":[{"message":{"content":"ok"}}]}`

	if err := service.TestCandidate(context.Background(), ProviderNVIDIA, "vendor/fresh-model"); err != nil {
		t.Fatalf("TestCandidate: %v", err)
	}
	if len(secrets.usedKeyIDs) != 1 {
		t.Fatalf("keys tried = %v, want exactly one", secrets.usedKeyIDs)
	}
	assertCandidateNotPersisted(t, db, "vendor/fresh-model")
}

func TestCandidateReportsWhenNoNVIDIAKeyIsAvailable(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)

	if err := service.TestCandidate(context.Background(), ProviderNVIDIA, "vendor/keyless"); !errors.Is(err, ErrNVIDIAKeyRequired) {
		t.Fatalf("error = %v, want ErrNVIDIAKeyRequired", err)
	}
}

// A candidate sweep must not burn NVIDIA keys on OpenCodeFree models, and the
// probe body must name the candidate's own upstream id.
func TestCandidateDispatchesOpenCodeFreeWithoutNVIDIAKeys(t *testing.T) {
	service, db, secrets, _ := newCatalogTestService(t)
	gateway := &fakeOpenCodeFreeGateway{chatResponse: `{"choices":[{"message":{"content":"ok"}}]}`}
	service = service.WithOpenCodeFree(gateway)

	if err := service.TestCandidate(context.Background(), ProviderOpenCodeFree, "mimo-v2.5-free"); err != nil {
		t.Fatalf("TestCandidate: %v", err)
	}
	if len(secrets.usedKeyIDs) != 0 {
		t.Fatalf("OpenCodeFree candidate probe consumed NVIDIA keys %v", secrets.usedKeyIDs)
	}
	if gateway.chatCalls != 1 {
		t.Fatalf("gateway calls = %d, want one probe", gateway.chatCalls)
	}
	var probe struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(gateway.chatBodies[0], &probe); err != nil {
		t.Fatalf("decode probe body: %v", err)
	}
	if probe.Model != "mimo-v2.5-free" {
		t.Fatalf("probe model = %q, want the candidate's upstream id", probe.Model)
	}
	// A one-token budget made free-tier models answer 200 with empty content,
	// which payload validation rightly rejects — working models were condemned
	// as unreachable. 16 tokens stayed too small: gateway chat-template tokens
	// are invisible but still billed, so the OCF base probe carries its own
	// larger window instead of matching the NVIDIA budget.
	if probe.MaxTokens != ocfProbeMaxTokens {
		t.Fatalf("probe max_tokens = %d, want %d", probe.MaxTokens, ocfProbeMaxTokens)
	}
	assertCandidateNotPersisted(t, db, "mimo-v2.5-free")
}

func TestCandidateRejectsUnknownProvider(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)

	if err := service.TestCandidate(context.Background(), "legacy-gateway", "vendor/x"); !errors.Is(err, ErrProviderNotRoutable) {
		t.Fatalf("error = %v, want ErrProviderNotRoutable", err)
	}
}

func assertCandidateNotPersisted(t *testing.T, db *sql.DB, upstreamID string) {
	t.Helper()
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM models WHERE upstream_id = ?`, upstreamID).Scan(&rows); err != nil {
		t.Fatalf("count models: %v", err)
	}
	if rows != 0 {
		t.Fatalf("candidate probe persisted %d rows for %q", rows, upstreamID)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runOnceForTest exposes one synchronous probe cycle for tests.
func (r *CapabilityProbeRunner) runOnceForTest(ctx context.Context) {
	cycle, err := r.runOnce(ctx)
	if err != nil {
		r.logger.Warn("capability probe cycle failed", "error", err)
		return
	}
	r.logger.Info("capability probe cycle completed",
		"models", cycle.total, "supported", cycle.supported,
		"unsupported", cycle.unsupported, "unknown", cycle.unknown)
}

// The startup consistency check must flag the llama shape — levels=[none] with
// zero_allowed=false yields an empty available-level set, which made every
// reasoning_effort request fail 501 model_capability_unsupported (2026-08-25).
func TestCountUnexpressibleReasoningProfilesFlagsLlamaShape(t *testing.T) {
	service, _, _, _ := newCatalogTestService(t)
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "healthy-reasoning", UpstreamID: "vendor/hr", DisplayName: "HR", Kind: KindChat,
		SupportsReasoning: true, ReasoningWireFormat: "openai",
		ReasoningLevels: []string{"none", "low", "high"}, ReasoningZeroAllowed: true,
	}}); err != nil {
		t.Fatalf("SaveSelection healthy: %v", err)
	}
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: "broken-reasoning", UpstreamID: "vendor/br", DisplayName: "BR", Kind: KindChat,
		SupportsReasoning: true, ReasoningWireFormat: "openai",
		ReasoningLevels: []string{"none"}, ReasoningZeroAllowed: false,
	}}); err != nil {
		t.Fatalf("SaveSelection broken: %v", err)
	}

	count, ids, err := service.CountUnexpressibleReasoningProfiles(context.Background())
	if err != nil {
		t.Fatalf("CountUnexpressibleReasoningProfiles: %v", err)
	}
	if count != 1 || len(ids) != 1 || ids[0] != "broken-reasoning" {
		t.Fatalf("count = %d ids = %v, want 1 [broken-reasoning]", count, ids)
	}
}

// detailedProbeService prepares a chat model for the detailed probe path.
func detailedProbeService(t *testing.T, publicID string) (*Service, *fakeDiscoverer, int64) {
	t.Helper()
	service, db, secrets, discoverer := newCatalogTestService(t)
	secrets.availableIDs = []int64{11}
	if err := service.SaveSelection(context.Background(), []Selection{{
		PublicID: publicID, UpstreamID: "vendor/" + publicID, DisplayName: publicID, Kind: KindChat,
	}}); err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	discoverer.chatResponse = `{"choices":[{"message":{"content":"ok"}}]}`
	return service, discoverer, modelIDByPublicID(t, db, publicID)
}

func serviceRepositoryDB(t *testing.T, service *Service) *sql.DB {
	t.Helper()
	return service.repository.db
}

// A gateway that ignores tool_choice:"required" used to stay unknown forever
// and deadlock every tools request behind 501. The auto fallback with an
// explicit instruction is what rescues them.
func TestToolsProbeTriesAutoFallbackWhenRequiredYieldsNoCalls(t *testing.T) {
	service, discoverer, modelID := detailedProbeService(t, "tools-fallback")
	discoverer.chatResponses = []string{
		`{"choices":[{"message":{"content":"ok"}}]}`, // base
		`{"choices":[{"message":{"content":"ok"}}]}`, // reasoning (openai wire)
		`{"choices":[{"message":{"content":"ok"}}]}`, // required form: silent
		toolsProbeCallBody,                           // auto form: calls
	}

	summary, err := service.TestModelAutoDetailed(context.Background(), modelID)
	if err != nil {
		t.Fatalf("TestModelAutoDetailed: %v", err)
	}
	if summary.Tools != "supported" {
		t.Fatalf("probe.Tools = %q, want supported", summary.Tools)
	}
	if len(discoverer.chatBodies) != 4 {
		t.Fatalf("chat calls = %d, want 4", len(discoverer.chatBodies))
	}
	var requiredForm map[string]any
	if err := json.Unmarshal(discoverer.chatBodies[2], &requiredForm); err != nil {
		t.Fatalf("decode required form: %v", err)
	}
	if requiredForm["tool_choice"] != "required" || requiredForm["max_tokens"] != float64(toolsProbeMaxTokens) {
		t.Fatalf("required form = %v/%v, want required/%d", requiredForm["tool_choice"], requiredForm["max_tokens"], toolsProbeMaxTokens)
	}
	var autoForm map[string]any
	if err := json.Unmarshal(discoverer.chatBodies[3], &autoForm); err != nil {
		t.Fatalf("decode auto form: %v", err)
	}
	if autoForm["tool_choice"] != "auto" {
		t.Fatalf("fallback tool_choice = %v, want auto", autoForm["tool_choice"])
	}
	messages, _ := autoForm["messages"].([]any)
	if len(messages) != 1 || !strings.Contains(messages[0].(map[string]any)["content"].(string), "must call the weather tool") {
		t.Fatalf("fallback message = %v, want explicit instruction", messages)
	}
	var stored string
	db := serviceRepositoryDB(t, service)
	if err := db.QueryRow(`SELECT tools_status FROM models WHERE id = ?`, modelID).Scan(&stored); err != nil {
		t.Fatalf("load stored status: %v", err)
	}
	if stored != ToolsStatusSupported {
		t.Fatalf("stored tools_status = %q, want supported", stored)
	}
}

// Both forms staying silent is the only shape that condemns a model to
// unsupported — one silent form may just be a rejected spelling.
func TestToolsProbeWritesUnsupportedOnlyWhenBothFormsStaySilent(t *testing.T) {
	service, discoverer, modelID := detailedProbeService(t, "tools-silent")
	discoverer.chatResponses = []string{
		`{"choices":[{"message":{"content":"ok"}}]}`,
		`{"choices":[{"message":{"content":"ok"}}]}`,
		`{"choices":[{"message":{"content":"ok"}}]}`,
		`{"choices":[{"message":{"content":"ok"}}]}`,
	}

	summary, err := service.TestModelAutoDetailed(context.Background(), modelID)
	if err != nil {
		t.Fatalf("TestModelAutoDetailed: %v", err)
	}
	if summary.Tools != ProbeStatusUnsupported {
		t.Fatalf("probe.Tools = %q, want unsupported", summary.Tools)
	}
	var stored string
	if err := serviceRepositoryDB(t, service).QueryRow(`SELECT tools_status FROM models WHERE id = ?`, modelID).Scan(&stored); err != nil {
		t.Fatalf("load stored status: %v", err)
	}
	if stored != ToolsStatusUnsupported {
		t.Fatalf("stored tools_status = %q, want unsupported", stored)
	}
}

// A transient failure on the first form stops the probe without firing the
// second form at an upstream that is visibly failing.
func TestToolsProbeStopsAfterTransientWithoutFallback(t *testing.T) {
	service, discoverer, modelID := detailedProbeService(t, "tools-transient")
	discoverer.chatStatuses = []int{200, 200, 503} // base, reasoning, required-form transient

	summary, err := service.TestModelAutoDetailed(context.Background(), modelID)
	if err != nil {
		t.Fatalf("TestModelAutoDetailed: %v", err)
	}
	if summary.Tools != ProbeStatusUnknown {
		t.Fatalf("probe.Tools = %q, want unknown", summary.Tools)
	}
	if len(discoverer.chatBodies) != 3 {
		t.Fatalf("chat calls = %d, want 3 (no auto fallback after transient)", len(discoverer.chatBodies))
	}
	var verifiedAt sql.NullString
	if err := serviceRepositoryDB(t, service).QueryRow(`SELECT tools_verified_at FROM models WHERE id = ?`, modelID).Scan(&verifiedAt); err != nil {
		t.Fatalf("load verified_at: %v", err)
	}
	if verifiedAt.Valid {
		t.Fatal("unknown verdict must not write tools_verified_at")
	}
}

// The required form being rejected outright (4xx) does not condemn the model:
// some gateways only accept the auto spelling.
func TestToolsProbeFourXXOnRequiredFallsBackToAuto(t *testing.T) {
	service, discoverer, modelID := detailedProbeService(t, "tools-4xx")
	// chatResponses pairs with chatStatuses: base 200, reasoning 200,
	// required-form 400, auto-form 200 with calls.
	discoverer.chatStatuses = []int{200, 200, 400, 200}
	discoverer.chatResponses = []string{
		`{"choices":[{"message":{"content":"ok"}}]}`,
		`{"choices":[{"message":{"content":"ok"}}]}`,
		`{"error":{"message":"tool_choice required not supported"}}`,
		toolsProbeCallBody,
	}

	summary, err := service.TestModelAutoDetailed(context.Background(), modelID)
	if err != nil {
		t.Fatalf("TestModelAutoDetailed: %v", err)
	}
	if summary.Tools != "supported" {
		t.Fatalf("probe.Tools = %q, want supported (auto form rescued)", summary.Tools)
	}
	_ = service
}
