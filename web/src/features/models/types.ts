export type ModelKind = 'chat' | 'embedding' | 'asr' | 'tts' | string
export type ModelProvider = 'nvidia' | 'opencodefree' | string
export type ToolsStatus = 'unknown' | 'inferred' | 'supported' | 'unsupported' | string
export type ModelTestMode = 'sequential' | 'concurrent'
export type ModelTestJobStatus =
  | 'queued'
  | 'running'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'canceled'
  | string

export interface ModelCapabilities {
  supports_vision: boolean
  supports_tools: boolean
  tools_status?: ToolsStatus
  tools_verified_at?: string
  supports_reasoning: boolean
  reasoning_status?: string
  reasoning_wire_format?: string
}

export interface Candidate extends ModelCapabilities {
  upstream_id: string
  display_name: string
  kind: ModelKind
  provider?: ModelProvider
  channel?: string
  badge?: string
  status?: string
  public_id?: string
  capabilities?: string[]
}

export interface Model extends Candidate {
  id: number
  provider?: ModelProvider
  public_id: string
  enabled: boolean
  capability_verified_at?: string
  // Per-model overrides of the global streaming windows; null/undefined means
  // "use the global setting".
  stream_first_token_timeout_ms?: number
  stream_idle_timeout_ms?: number
  // Operator-declared context window (tokens); 0/undefined means undeclared and
  // /v1/models omits the field so clients fall back to their own defaults.
  context_length?: number
  blocked_by_key_ids?: number[]
}

export interface SaveSelection extends Candidate {
  public_id: string
  enabled: boolean
}

// One job may mix providers: the backend dispatches each model on its own
// provider and picks the credential or proxy exit itself.
export interface ModelTestJobRequest {
  model_ids: number[]
  mode: ModelTestMode
  concurrency: number
}

export interface ModelTestResult {
  model_id: number
  public_id?: string
  provider?: ModelProvider
  status: string
  duration_ms?: number
  probe?: {
    base: string
    reasoning: string
    reasoning_wire_format?: string
    tools: string
  }
  error?: string
  started_at?: string
  finished_at?: string
}

export interface ModelTestJob {
  id: string | number
  mode: ModelTestMode
  status: ModelTestJobStatus
  total: number
  completed: number
  results: ModelTestResult[]
  error?: string
  created_at?: string
  started_at?: string
  finished_at?: string
}

export interface ModelPatch {
  provider?: string
  display_name?: string
  kind?: ModelKind
  enabled?: boolean
  supports_vision?: boolean
  supports_tools?: boolean
  supports_reasoning?: boolean
  reasoning_wire_format?: string
  stream_first_token_timeout_ms?: number
  stream_idle_timeout_ms?: number
  context_length?: number
}

export interface CandidatesResponse {
  data: Candidate[]
}

export interface ModelsResponse {
  data: Model[]
}

export interface CandidateTestRequest {
  provider: ModelProvider
  upstream_id: string
}

export interface CandidateTestResult {
  status: 'success' | 'failed' | string
  duration_ms: number
  error?: string
}

// Per-candidate UI state for the read-only probe; keyed by candidateSelectionKey
// and kept in page state only — a refresh clears it, nothing is persisted.
export interface CandidateTestState {
  status: 'running' | 'success' | 'failed'
  duration_ms?: number
  error?: string
}

export function formatTestDuration(durationMs?: number): string {
  if (durationMs === undefined) return '—'
  if (durationMs < 1000) return `${Math.round(durationMs)} ms`
  return `${(durationMs / 1000).toFixed(1)} s`
}

export function candidateTestSummary(state?: CandidateTestState): string {
  if (!state || state.status === 'running') return '测试中…'
  if (state.status === 'success') return `可用 · ${formatTestDuration(state.duration_ms)}`
  return `不可用${state.error ? ` · ${state.error}` : ''}`
}

export function normalizeProvider(provider?: string): ModelProvider {
  return provider?.trim().toLowerCase() || 'nvidia'
}

export function candidatePublicId(candidate: Candidate): string {
  if (candidate.public_id?.trim()) return candidate.public_id
  const provider = normalizeProvider(candidate.provider)
  return provider === 'opencodefree'
    ? `opencodefree/${candidate.upstream_id}`
    : candidate.upstream_id
}

export function candidateSelectionKey(candidate: Candidate): string {
  return candidatePublicId(candidate)
}

export function capabilityLabels(model: ModelCapabilities & { capabilities?: string[] }): string[] {
  const labels = [...(model.capabilities ?? [])]
  if (model.supports_vision) labels.push('vision')
  if (model.supports_tools) labels.push('tools')
  if (model.supports_reasoning) labels.push('reasoning')
  return [...new Set(labels.map((label) => label.trim()).filter(Boolean))]
}

export function toolsStatusLabel(status?: ToolsStatus): string {
  switch (status) {
    case 'supported': return 'Tools 已验证'
    case 'inferred': return 'Tools 推断'
    case 'unsupported': return 'Tools 不支持'
    default: return 'Tools 未知'
  }
}
