package modelcatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"nvidia-router/internal/keystate"
)

type Repository struct {
	db     *sql.DB
	reader *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// WithReader routes read-only queries to a separate connection pool. The writer
// pool is capped at one connection, so without this every per-request model
// resolution queued behind in-flight writes.
func (r *Repository) WithReader(reader *sql.DB) *Repository {
	clone := *r
	clone.reader = reader
	return &clone
}

func (r *Repository) read() *sql.DB {
	if r.reader != nil {
		return r.reader
	}
	return r.db
}

func (r *Repository) SaveSelections(ctx context.Context, selections []Selection, now time.Time) (returnErr error) {
	_, err := r.SaveSelectionsResult(ctx, selections, now)
	return err
}

func (r *Repository) SaveSelectionsResult(ctx context.Context, selections []Selection, now time.Time) (result MutationResult, returnErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, fmt.Errorf("begin model selection transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = fmt.Errorf("rollback model selection transaction: %w", errors.Join(returnErr, rollbackErr))
		}
	}()

	result.PreviousKinds = make(map[int64]Kind, len(selections))
	for _, selection := range selections {
		model, previousKind, err := saveSelection(ctx, tx, selection, now)
		if err != nil {
			return MutationResult{}, err
		}
		if previousKind != "" {
			result.PreviousKinds[model.ID] = previousKind
		}
		result.Models = append(result.Models, model)
	}
	if err := tx.Commit(); err != nil {
		return MutationResult{}, fmt.Errorf("commit model selection transaction: %w", err)
	}
	committed = true
	return result, nil
}

func saveSelection(ctx context.Context, tx *sql.Tx, selection Selection, now time.Time) (Model, Kind, error) {
	if selection.Provider == "" {
		selection.Provider = defaultModelProvider
	}
	if selection.ToolsStatus == "" {
		if selection.SupportsTools {
			selection.ToolsStatus = ToolsStatusInferred
		} else {
			selection.ToolsStatus = ToolsStatusUnknown
		}
	}
	// Repository callers include migration fixtures and import paths that may not
	// pass through Service.SaveSelection. Keep the persisted status valid for
	// those callers instead of sending an empty string past the database CHECK.
	if selection.ReasoningStatus == "" {
		if selection.SupportsReasoning {
			selection.ReasoningStatus = ReasoningStatusInferred
		} else {
			selection.ReasoningStatus = ReasoningStatusUnknown
		}
	}
	var previousProvider sql.NullString
	var previousKind sql.NullString
	var previousUpdatedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT provider, kind, updated_at FROM models WHERE public_id = ?`, selection.PublicID).Scan(&previousProvider, &previousKind, &previousUpdatedAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Model{}, "", fmt.Errorf("load existing model revision %q: %w", selection.PublicID, err)
	}
	if err := validateEnabledProvider(selection.Provider, selection.Enabled); err != nil {
		return Model{}, "", fmt.Errorf("validate model provider %q: %w", selection.PublicID, err)
	}
	// Enabling a row whose stored provider is not one this build supports would let
	// the upsert below silently rewrite it to the incoming provider. The check must
	// only fire on an actual error: it used to wrap the result with %w
	// unconditionally, and %w on a nil error still yields a non-nil error, so a
	// stored OpenCodeFree model — which validateEnabledProvider accepts — could
	// never be re-enabled, and the whole batch transaction rolled back with it.
	if previousProvider.Valid && previousProvider.String != defaultModelProvider && selection.Enabled {
		if err := validateEnabledProvider(previousProvider.String, selection.Enabled); err != nil {
			return Model{}, "", fmt.Errorf("validate existing model provider %q: %w", selection.PublicID, err)
		}
	}
	if previousKind.Valid && Kind(previousKind.String) != selection.Kind {
		if _, err := tx.ExecContext(ctx, `DELETE FROM nvidia_key_model_blocks WHERE model_id = (SELECT id FROM models WHERE public_id = ?)`, selection.PublicID); err != nil {
			return Model{}, "", fmt.Errorf("clear model blocks after kind change %q: %w", selection.PublicID, err)
		}
	}
	verifiedAt := optionalTimestamp(selection.CapabilityVerifiedAt)
	updatedAt := nextUpdatedAt(now, previousUpdatedAt)
	createdAt := formatTimestamp(now)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO models (
			public_id, upstream_id, display_name, kind, provider, enabled,
			supports_vision, supports_tools, tools_status, tools_verified_at, supports_reasoning, reasoning_status,
			reasoning_wire_format, reasoning_levels, reasoning_min_budget, reasoning_max_budget,
			reasoning_zero_allowed, reasoning_dynamic_allowed, capability_verified_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(public_id) DO UPDATE SET
			upstream_id = excluded.upstream_id,
			display_name = excluded.display_name,
			kind = excluded.kind,
			provider = excluded.provider,
			enabled = excluded.enabled,
			supports_vision = excluded.supports_vision,
			supports_tools = CASE WHEN models.tools_status IN ('supported', 'unsupported') THEN models.supports_tools ELSE excluded.supports_tools END,
			tools_status = CASE WHEN models.tools_status IN ('supported', 'unsupported') THEN models.tools_status ELSE excluded.tools_status END,
			tools_verified_at = CASE WHEN models.tools_status IN ('supported', 'unsupported') THEN models.tools_verified_at ELSE excluded.tools_verified_at END,
			supports_reasoning = CASE WHEN models.reasoning_status IN ('visible', 'hidden', 'unsupported') THEN models.supports_reasoning ELSE excluded.supports_reasoning END,
			reasoning_status = CASE WHEN models.reasoning_status IN ('visible', 'hidden', 'unsupported') THEN models.reasoning_status ELSE excluded.reasoning_status END,
			reasoning_wire_format = CASE WHEN models.reasoning_status IN ('visible', 'hidden', 'unsupported') THEN models.reasoning_wire_format ELSE excluded.reasoning_wire_format END,
			reasoning_levels = excluded.reasoning_levels,
			reasoning_min_budget = excluded.reasoning_min_budget,
			reasoning_max_budget = excluded.reasoning_max_budget,
			reasoning_zero_allowed = excluded.reasoning_zero_allowed,
			reasoning_dynamic_allowed = excluded.reasoning_dynamic_allowed,
			capability_verified_at = excluded.capability_verified_at,
			updated_at = excluded.updated_at
	`, selection.PublicID, selection.UpstreamID, selection.DisplayName, selection.Kind, selection.Provider, boolInt(selection.Enabled),
		boolInt(selection.SupportsVision), boolInt(selection.SupportsTools), selection.ToolsStatus, optionalTimestamp(selection.ToolsVerifiedAt), boolInt(selection.SupportsReasoning), selection.ReasoningStatus,
		selection.ReasoningWireFormat, mustReasoningLevelsJSON(selection.ReasoningLevels), selection.ReasoningMinBudget,
		selection.ReasoningMaxBudget, boolInt(selection.ReasoningZeroAllowed), boolInt(selection.ReasoningDynamicAllowed),
		verifiedAt, createdAt, updatedAt); err != nil {
		return Model{}, "", fmt.Errorf("save model %q: %w", selection.PublicID, err)
	}
	model, err := scanModel(tx.QueryRowContext(ctx, modelColumns+" WHERE public_id = ?", selection.PublicID))
	if err != nil {
		return Model{}, "", fmt.Errorf("load saved model %q: %w", selection.PublicID, err)
	}
	if err := attachBlockedKeyIDsTx(ctx, tx, &model); err != nil {
		return Model{}, "", err
	}
	if previousKind.Valid {
		return model, Kind(previousKind.String), nil
	}
	return model, "", nil
}

func (r *Repository) Patch(ctx context.Context, id int64, patch Patch, now time.Time) (model Model, previousKind Kind, returnErr error) {
	if patch.ContextLength != nil && *patch.ContextLength < 0 {
		return Model{}, "", fmt.Errorf("%w: context_length must be non-negative", ErrInvalidModelSelection)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Model{}, "", fmt.Errorf("begin model patch transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = fmt.Errorf("rollback model patch transaction: %w", errors.Join(returnErr, rollbackErr))
		}
	}()
	model, err = scanModel(tx.QueryRowContext(ctx, modelColumns+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, "", ErrModelNotFound
	}
	if err != nil {
		return Model{}, "", fmt.Errorf("load model for patch: %w", err)
	}
	previousKind = model.Kind
	selection := selectionFromModel(model)
	applyPatch(&selection, patch)
	provider := model.Provider
	if patch.Provider != nil {
		provider = *patch.Provider
	}
	selection.Provider = provider
	if model.Kind != selection.Kind {
		selection.CapabilityVerifiedAt = nil
		if _, err := tx.ExecContext(ctx, "DELETE FROM nvidia_key_model_blocks WHERE model_id = ?", id); err != nil {
			return Model{}, "", fmt.Errorf("clear model blocks after patch kind change: %w", err)
		}
	}
	selection, err = normalizeModelSelection(selection)
	if err != nil {
		return Model{}, "", fmt.Errorf("validate model patch: %w", err)
	}
	if err := validateEnabledProvider(provider, selection.Enabled); err != nil {
		return Model{}, "", fmt.Errorf("validate model patch provider: %w", err)
	}
	updatedAt := formatRevisionTime(now, model.updatedAt)
	result, err := tx.ExecContext(ctx, `UPDATE models SET upstream_id = ?, display_name = ?, kind = ?, enabled = ?, supports_vision = ?, supports_tools = ?, tools_status = ?, tools_verified_at = ?, supports_reasoning = ?, reasoning_status = ?, reasoning_wire_format = ?, reasoning_levels = ?, reasoning_min_budget = ?, reasoning_max_budget = ?, reasoning_zero_allowed = ?, reasoning_dynamic_allowed = ?, capability_verified_at = ?,
			provider = CASE WHEN ? IS NULL THEN provider ELSE ? END,
			stream_first_token_timeout_ms = CASE WHEN ? IS NULL THEN stream_first_token_timeout_ms ELSE ? END,
			stream_idle_timeout_ms        = CASE WHEN ? IS NULL THEN stream_idle_timeout_ms        ELSE ? END,
			context_length                = CASE WHEN ? IS NULL THEN context_length                ELSE ? END,
			updated_at = ? WHERE id = ?`,
		selection.UpstreamID, selection.DisplayName, selection.Kind, boolInt(selection.Enabled), boolInt(selection.SupportsVision), boolInt(selection.SupportsTools), selection.ToolsStatus, optionalTimestamp(selection.ToolsVerifiedAt), boolInt(selection.SupportsReasoning), selection.ReasoningStatus, selection.ReasoningWireFormat, mustReasoningLevelsJSON(selection.ReasoningLevels), selection.ReasoningMinBudget, selection.ReasoningMaxBudget, boolInt(selection.ReasoningZeroAllowed), boolInt(selection.ReasoningDynamicAllowed), optionalTimestamp(selection.CapabilityVerifiedAt),
		patch.Provider, patchDerefString(patch.Provider),
		patch.StreamFirstTokenTimeoutMS, patchDerefInt(patch.StreamFirstTokenTimeoutMS),
		patch.StreamIdleTimeoutMS, patchDerefInt(patch.StreamIdleTimeoutMS),
		patch.ContextLength, patchDerefInt(patch.ContextLength),
		updatedAt, id)
	if err != nil {
		return Model{}, "", fmt.Errorf("save model patch: %w", err)
	}
	if err := requireOneRow(result, "patch model"); err != nil {
		return Model{}, "", err
	}
	model, err = scanModel(tx.QueryRowContext(ctx, modelColumns+" WHERE id = ?", id))
	if err != nil {
		return Model{}, "", fmt.Errorf("load patched model: %w", err)
	}
	if err := attachBlockedKeyIDsTx(ctx, tx, &model); err != nil {
		return Model{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Model{}, "", fmt.Errorf("commit model patch transaction: %w", err)
	}
	committed = true
	return model, previousKind, nil
}

func (r *Repository) List(ctx context.Context) ([]Model, error) {
	rows, err := r.read().QueryContext(ctx, modelColumns+" ORDER BY public_id")
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	models := make([]Model, 0)
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		model.BlockedByKeyIDs = []int64{}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate models: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close model rows: %w", err)
	}
	if err := r.attachBlockedKeyIDs(ctx, models); err != nil {
		return nil, err
	}
	return models, nil
}

func (r *Repository) attachBlockedKeyIDs(ctx context.Context, models []Model) error {
	indexes := make(map[int64]int, len(models))
	for index := range models {
		indexes[models[index].ID] = index
	}
	rows, err := r.read().QueryContext(ctx, `
		SELECT model_id, nvidia_key_id
		FROM nvidia_key_model_blocks
		ORDER BY model_id, nvidia_key_id
	`)
	if err != nil {
		return fmt.Errorf("list model key blocks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var modelID, keyID int64
		if err := rows.Scan(&modelID, &keyID); err != nil {
			return fmt.Errorf("scan model key block: %w", err)
		}
		if index, ok := indexes[modelID]; ok {
			models[index].BlockedByKeyIDs = append(models[index].BlockedByKeyIDs, keyID)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate model key blocks: %w", err)
	}
	return nil
}

func attachBlockedKeyIDsTx(ctx context.Context, tx *sql.Tx, model *Model) error {
	rows, err := tx.QueryContext(ctx, "SELECT nvidia_key_id FROM nvidia_key_model_blocks WHERE model_id = ? ORDER BY nvidia_key_id", model.ID)
	if err != nil {
		return fmt.Errorf("list model key blocks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	model.BlockedByKeyIDs = []int64{}
	for rows.Next() {
		var keyID int64
		if err := rows.Scan(&keyID); err != nil {
			return fmt.Errorf("scan model key block: %w", err)
		}
		model.BlockedByKeyIDs = append(model.BlockedByKeyIDs, keyID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate model key blocks: %w", err)
	}
	return nil
}

func (r *Repository) ListEnabled(ctx context.Context) ([]Model, error) {
	rows, err := r.read().QueryContext(ctx, modelColumns+" WHERE enabled = 1 ORDER BY public_id")
	if err != nil {
		return nil, fmt.Errorf("list enabled models: %w", err)
	}
	defer func() { _ = rows.Close() }()
	models := make([]Model, 0)
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate enabled models: %w", err)
	}
	return models, nil
}

func (r *Repository) ResolveEnabled(ctx context.Context, publicID string) (Model, error) {
	model, err := scanModel(r.read().QueryRowContext(ctx, modelColumns+" WHERE public_id = ? AND enabled = 1", publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, ErrModelNotFound
	}
	if err != nil {
		return Model{}, fmt.Errorf("resolve enabled model: %w", err)
	}
	return model, nil
}

func (r *Repository) Get(ctx context.Context, id int64) (Model, error) {
	model, err := scanModel(r.read().QueryRowContext(ctx, modelColumns+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, ErrModelNotFound
	}
	if err != nil {
		return Model{}, fmt.Errorf("get model: %w", err)
	}
	return model, nil
}

func (r *Repository) SetEnabled(ctx context.Context, id int64, enabled bool, now time.Time) (returnErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin model enabled transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = fmt.Errorf("rollback model enabled transaction: %w", errors.Join(returnErr, rollbackErr))
		}
	}()
	model, err := scanModel(tx.QueryRowContext(ctx, modelColumns+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrModelNotFound
	}
	if err != nil {
		return fmt.Errorf("load model before setting enabled state: %w", err)
	}
	if err := validateEnabledProvider(model.Provider, enabled); err != nil {
		return err
	}
	if enabled && requiresVerification(model.Kind) && model.CapabilityVerifiedAt == nil {
		return ErrCapabilityUnverified
	}
	result, err := tx.ExecContext(ctx, "UPDATE models SET enabled = ?, updated_at = ? WHERE id = ?", boolInt(enabled), formatRevisionTime(now, model.updatedAt), id)
	if err != nil {
		return fmt.Errorf("set model enabled state: %w", err)
	}
	if err := requireOneRow(result, "set model enabled state"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit model enabled transaction: %w", err)
	}
	committed = true
	return nil
}

func (r *Repository) SetCapabilityVerified(ctx context.Context, id int64, verifiedAt *time.Time, now time.Time) (returnErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin model capability transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = fmt.Errorf("rollback model capability transaction: %w", errors.Join(returnErr, rollbackErr))
		}
	}()
	model, err := scanModel(tx.QueryRowContext(ctx, modelColumns+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrModelNotFound
	}
	if err != nil {
		return fmt.Errorf("load model before setting capability verification: %w", err)
	}
	disable := verifiedAt == nil && requiresVerification(model.Kind)
	result, err := tx.ExecContext(ctx, `
		UPDATE models
		SET capability_verified_at = ?,
		    enabled = CASE WHEN ? = 1 THEN 0 ELSE enabled END,
		    updated_at = ?
		WHERE id = ?
	`, optionalTimestamp(verifiedAt), boolInt(disable), formatRevisionTime(now, model.updatedAt), id)
	if err != nil {
		return fmt.Errorf("set model capability verification: %w", err)
	}
	if err := requireOneRow(result, "set model capability verification"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit model capability transaction: %w", err)
	}
	committed = true
	return nil
}

func (r *Repository) DeleteModel(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM models WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	return nil
}

func (r *Repository) BlockKeyModel(ctx context.Context, keyID, modelID int64, reason string, upstreamStatus *int, now time.Time) error {
	timestamp := formatTimestamp(now)
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO nvidia_key_model_blocks (
			nvidia_key_id, model_id, reason_code, upstream_status, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(nvidia_key_id, model_id) DO UPDATE SET
			reason_code = excluded.reason_code,
			upstream_status = excluded.upstream_status,
			last_seen_at = excluded.last_seen_at
	`, keyID, modelID, reason, upstreamStatus, timestamp, timestamp); err != nil {
		return fmt.Errorf("block NVIDIA key for model: %w", err)
	}
	return nil
}

func (r *Repository) VerifyAndUnblock(ctx context.Context, keyID, modelID int64, expectedUpdatedAt, verifiedAt time.Time) (model Model, returnErr error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Model{}, fmt.Errorf("begin model verification transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = fmt.Errorf("rollback model verification transaction: %w", errors.Join(returnErr, rollbackErr))
		}
	}()
	model, err = scanModel(tx.QueryRowContext(ctx, modelColumns+" WHERE id = ?", modelID))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, ErrModelNotFound
	}
	if err != nil {
		return Model{}, fmt.Errorf("load model for verification: %w", err)
	}
	if !model.updatedAt.Equal(expectedUpdatedAt) {
		return Model{}, ErrModelVersionConflict
	}
	if requiresVerification(model.Kind) {
		result, err := tx.ExecContext(ctx, `UPDATE models SET capability_verified_at = ?, updated_at = ? WHERE id = ? AND updated_at = ?`, formatTimestamp(verifiedAt), formatRevisionTime(verifiedAt, model.updatedAt), modelID, model.updatedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return Model{}, fmt.Errorf("mark model capability verified: %w", err)
		}
		if err := requireConditionalModelRow(ctx, tx, result, modelID, "mark model capability verified"); err != nil {
			return Model{}, err
		}
		verified := verifiedAt.UTC().Truncate(time.Second)
		model.CapabilityVerifiedAt = &verified
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM nvidia_key_model_blocks WHERE nvidia_key_id = ? AND model_id = ?`, keyID, modelID)
	if err != nil {
		return Model{}, fmt.Errorf("clear NVIDIA key model block: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Model{}, fmt.Errorf("commit model verification transaction: %w", err)
	}
	committed = true
	return model, nil
}

func (r *Repository) UnblockKeyModel(ctx context.Context, keyID, modelID int64) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM nvidia_key_model_blocks WHERE nvidia_key_id = ? AND model_id = ?", keyID, modelID); err != nil {
		return fmt.Errorf("unblock NVIDIA key for model: %w", err)
	}
	return nil
}

func (r *Repository) ListBlocks(ctx context.Context) ([]keystate.ModelBlock, error) {
	rows, err := r.read().QueryContext(ctx, "SELECT nvidia_key_id, model_id FROM nvidia_key_model_blocks ORDER BY nvidia_key_id, model_id")
	if err != nil {
		return nil, fmt.Errorf("list NVIDIA key model blocks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	blocks := make([]keystate.ModelBlock, 0)
	for rows.Next() {
		var block keystate.ModelBlock
		if err := rows.Scan(&block.KeyID, &block.ModelID); err != nil {
			return nil, fmt.Errorf("scan NVIDIA key model block: %w", err)
		}
		blocks = append(blocks, block)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate NVIDIA key model blocks: %w", err)
	}
	return blocks, nil
}

const modelColumns = `SELECT id, public_id, upstream_id, display_name, kind, provider, enabled,
		supports_vision, supports_tools, tools_status, tools_verified_at, supports_reasoning, reasoning_status, reasoning_wire_format,
		reasoning_levels, reasoning_min_budget, reasoning_max_budget, reasoning_zero_allowed, reasoning_dynamic_allowed,
		capability_verified_at, created_at, updated_at,
		stream_first_token_timeout_ms, stream_idle_timeout_ms,
		context_length FROM models`

type rowScanner interface{ Scan(dest ...any) error }

func selectionFromModel(model Model) Selection {
	return Selection{PublicID: model.PublicID, UpstreamID: model.UpstreamID, DisplayName: model.DisplayName, Kind: model.Kind, Provider: model.Provider, Enabled: model.Enabled, SupportsVision: model.SupportsVision, SupportsTools: model.SupportsTools, ToolsStatus: model.ToolsStatus, ToolsVerifiedAt: model.ToolsVerifiedAt, SupportsReasoning: model.SupportsReasoning, ReasoningStatus: model.ReasoningStatus, ReasoningWireFormat: model.ReasoningWireFormat, ReasoningLevels: append([]string(nil), model.ReasoningLevels...), ReasoningMinBudget: model.ReasoningMinBudget, ReasoningMaxBudget: model.ReasoningMaxBudget, ReasoningZeroAllowed: model.ReasoningZeroAllowed, ReasoningDynamicAllowed: model.ReasoningDynamicAllowed, CapabilityVerifiedAt: model.CapabilityVerifiedAt}
}

func applyPatch(selection *Selection, patch Patch) {
	if patch.DisplayName != nil {
		selection.DisplayName = *patch.DisplayName
	}
	if patch.Kind != nil {
		selection.Kind = *patch.Kind
	}
	if patch.Provider != nil {
		selection.Provider = *patch.Provider
	}
	if patch.Enabled != nil {
		selection.Enabled = *patch.Enabled
	}
	if patch.SupportsVision != nil {
		selection.SupportsVision = *patch.SupportsVision
	}
	if patch.SupportsTools != nil {
		selection.SupportsTools = *patch.SupportsTools
		selection.ToolsVerifiedAt = nil
		if *patch.SupportsTools {
			selection.ToolsStatus = ToolsStatusInferred
		} else {
			selection.ToolsStatus = ToolsStatusUnknown
		}
	}
	if patch.SupportsReasoning != nil {
		selection.SupportsReasoning = *patch.SupportsReasoning
		if patch.ReasoningStatus == nil {
			if *patch.SupportsReasoning {
				selection.ReasoningStatus = ReasoningStatusInferred
			} else {
				selection.ReasoningStatus = ReasoningStatusUnsupported
			}
		}
	}
	if patch.ReasoningStatus != nil {
		selection.ReasoningStatus = *patch.ReasoningStatus
	}
	if patch.ReasoningWireFormat != nil {
		selection.ReasoningWireFormat = *patch.ReasoningWireFormat
	}
	if patch.ReasoningLevels != nil {
		selection.ReasoningLevels = append([]string(nil), (*patch.ReasoningLevels)...)
	}
	if patch.ReasoningMinBudget != nil {
		selection.ReasoningMinBudget = *patch.ReasoningMinBudget
	}
	if patch.ReasoningMaxBudget != nil {
		selection.ReasoningMaxBudget = *patch.ReasoningMaxBudget
	}
	if patch.ReasoningZeroAllowed != nil {
		selection.ReasoningZeroAllowed = *patch.ReasoningZeroAllowed
	}
	if patch.ReasoningDynamicAllowed != nil {
		selection.ReasoningDynamicAllowed = *patch.ReasoningDynamicAllowed
	}
}

func scanModel(row rowScanner) (Model, error) {
	var model Model
	var enabled, vision, tools, reasoning int
	var zeroAllowed, dynamicAllowed int
	var toolsVerifiedAt, verifiedAt, createdAt, updatedAt sql.NullString
	var toolsStatus string
	var reasoningLevels string
	var reasoningMin, reasoningMax int
	var streamFirstToken, streamIdle sql.NullInt64
	if err := row.Scan(&model.ID, &model.PublicID, &model.UpstreamID, &model.DisplayName, &model.Kind,
		&model.Provider, &enabled, &vision, &tools, &toolsStatus, &toolsVerifiedAt, &reasoning, &model.ReasoningStatus, &model.ReasoningWireFormat, &reasoningLevels, &reasoningMin, &reasoningMax, &zeroAllowed, &dynamicAllowed, &verifiedAt, &createdAt, &updatedAt,
		&streamFirstToken, &streamIdle, &model.ContextLength); err != nil {
		return Model{}, err
	}
	if model.Provider == "" {
		model.Provider = defaultModelProvider
	}
	if model.ReasoningStatus == "" {
		if reasoning == 1 {
			model.ReasoningStatus = ReasoningStatusInferred
		} else {
			model.ReasoningStatus = ReasoningStatusUnknown
		}
	}
	model.Enabled = enabled == 1
	model.SupportsVision = vision == 1
	model.SupportsTools = tools == 1
	model.ToolsStatus = toolsStatus
	if model.ToolsStatus == "" {
		if model.SupportsTools {
			model.ToolsStatus = ToolsStatusInferred
		} else {
			model.ToolsStatus = ToolsStatusUnknown
		}
	}
	model.SupportsReasoning = reasoning == 1
	if reasoningLevels != "" {
		if err := json.Unmarshal([]byte(reasoningLevels), &model.ReasoningLevels); err != nil {
			return Model{}, fmt.Errorf("parse model reasoning levels: %w", err)
		}
	}
	model.ReasoningMinBudget = reasoningMin
	model.ReasoningMaxBudget = reasoningMax
	model.ReasoningZeroAllowed = zeroAllowed == 1
	model.ReasoningDynamicAllowed = dynamicAllowed == 1
	if toolsVerifiedAt.Valid {
		parsed, err := time.Parse(time.RFC3339, toolsVerifiedAt.String)
		if err != nil {
			return Model{}, fmt.Errorf("parse model tools verification time: %w", err)
		}
		model.ToolsVerifiedAt = &parsed
	}
	if verifiedAt.Valid {
		parsed, err := time.Parse(time.RFC3339, verifiedAt.String)
		if err != nil {
			return Model{}, fmt.Errorf("parse model capability verification time: %w", err)
		}
		model.CapabilityVerifiedAt = &parsed
	}
	if createdAt.Valid {
		parsed, err := time.Parse(time.RFC3339, createdAt.String)
		if err != nil {
			return Model{}, fmt.Errorf("parse model creation time: %w", err)
		}
		model.CreatedAt = parsed
	}
	if updatedAt.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, updatedAt.String)
		if err != nil {
			return Model{}, fmt.Errorf("parse model update time: %w", err)
		}
		model.updatedAt = parsed
	}
	if streamFirstToken.Valid {
		v := int(streamFirstToken.Int64)
		model.StreamFirstTokenTimeoutMS = &v
	}
	if streamIdle.Valid {
		v := int(streamIdle.Int64)
		model.StreamIdleTimeoutMS = &v
	}
	return model, nil
}

func mustReasoningLevelsJSON(levels []string) string {
	encoded, err := json.Marshal(levels)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func requireOneRow(result sql.Result, operation string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", operation, err)
	}
	if rows != 1 {
		return fmt.Errorf("%s: %w", operation, ErrModelNotFound)
	}
	return nil
}

func requireConditionalModelRow(ctx context.Context, tx *sql.Tx, result sql.Result, id int64, operation string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", operation, err)
	}
	if rows == 1 {
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM models WHERE id = ?", id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", operation, ErrModelNotFound)
	} else if err != nil {
		return fmt.Errorf("%s check model: %w", operation, err)
	}
	return fmt.Errorf("%s: %w", operation, ErrModelVersionConflict)
}

func nextUpdatedAt(now time.Time, previous sql.NullString) string {
	if !previous.Valid {
		return formatRevisionTimestamp(now)
	}
	old, err := time.Parse(time.RFC3339Nano, previous.String)
	if err != nil {
		return formatRevisionTimestamp(now)
	}
	return formatRevisionTime(now, old)
}

func formatRevisionTime(now, previous time.Time) string {
	revision := now.UTC().Truncate(time.Second)
	previous = previous.UTC()
	if !revision.After(previous) {
		revision = previous.Add(time.Nanosecond)
	}
	return revision.Format(time.RFC3339Nano)
}

func formatRevisionTimestamp(now time.Time) string {
	return now.UTC().Truncate(time.Second).Format(time.RFC3339Nano)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func optionalTimestamp(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTimestamp(*value)
}

// patchDerefString is the ELSE branch of the provider CASE: nil preserves the
// stored provider, a non-nil value replaces it.
func patchDerefString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// patchDerefInt is the ELSE branch of the stream-timeout CASE: nil preserves the
// stored value, a non-nil value replaces it.
func patchDerefInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func formatTimestamp(value time.Time) string {
	return value.UTC().Truncate(time.Second).Format(time.RFC3339)
}
