<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import { ApiError, isDataArrayResponse, isFiniteNumber, isRecord } from '../../shared/api/client'
import { toastError, toastSuccess } from '../../shared/toast'
import { useAsyncData } from '../../shared/useAsyncData'
import UiBadge from '../../shared/ui/UiBadge.vue'
import UiButton from '../../shared/ui/UiButton.vue'
import UiCard from '../../shared/ui/UiCard.vue'
import UiConfirmDialog from '../../shared/ui/UiConfirmDialog.vue'
import UiPageHeader from '../../shared/ui/UiPageHeader.vue'
import UiSelect from '../../shared/ui/UiSelect.vue'
import UiStatePanel from '../../shared/ui/UiStatePanel.vue'
import { modelsApi } from './api'
import ModelCards from './ModelCards.vue'
import ModelTable from './ModelTable.vue'
import {
  candidatePublicId,
  candidateSelectionKey,
  capabilityLabels,
  normalizeProvider,
} from './types'
import type {
  Candidate,
  CandidateTestState,
  Model,
  ModelTestJob,
  ModelTestJobRequest,
  ModelTestMode,
  ModelTestResult,
  SaveSelection,
} from './types'

const { data: models, loading, error: loadError, refresh: loadModels, isDisposed } = useAsyncData<Model[]>(
  async () => {
    const response: unknown = await modelsApi.list()
    if (!isDataArrayResponse(response, isModel)) {
      throw new TypeError('Invalid model list response.')
    }
    return response.data
  },
  { errorMessage: '模型列表加载失败。' },
)

const modelList = computed<Model[]>(() => models.value ?? [])
const candidates = ref<Candidate[]>([])
const selectedCandidates = ref<Record<string, boolean>>({})
const selectedTestModels = ref<Record<string, boolean>>({})
const candidateSearch = ref('')
const providerFilter = ref('all')
const testMode = ref<ModelTestMode>('concurrent')
const testConcurrency = ref(4)
const testJob = ref<ModelTestJob | null>(null)
const testJobError = ref('')
const testJobLoading = ref(false)
const testJobCancelling = ref(false)
const jobPollTimer = ref<ReturnType<typeof globalThis.setTimeout> | undefined>(undefined)
const jobPollToken = ref(0)

const discovering = ref(false)
const saving = ref(false)
const busyId = ref<number | null>(null)
const pendingDelete = ref<Model | null>(null)
const deleting = ref(false)
const errorMessage = ref('')
const candidateMessage = ref('')

// Candidate probes run read-only straight from the discovery payload, so their
// verdicts live only in this page state; a refresh or navigation drops them.
const candidateTestStates = ref<Record<string, CandidateTestState>>({})
const candidateBatchTesting = ref(false)
const candidateBatchStopRequested = ref(false)

const providerOptions = computed<string[]>(() => {
  const providers = new Set<string>(['nvidia'])
  for (const model of modelList.value) providers.add(normalizeProvider(model.provider))
  for (const candidate of candidates.value) providers.add(normalizeProvider(candidate.provider))
  return [...providers]
})

const filteredCandidates = computed<Candidate[]>(() => candidates.value.filter((candidate) => matchesFilter(candidate)))
const filteredModels = computed<Model[]>(() => modelList.value.filter((model) => matchesFilter(model)))
const selectedCandidateKeys = computed<ReadonlySet<string>>(() => new Set(
  Object.entries(selectedCandidates.value)
    .filter(([, selected]) => selected)
    .map(([key]) => key),
))
const selectedTestModelIds = computed<ReadonlySet<number>>(() => new Set(
  Object.entries(selectedTestModels.value)
    .filter(([, selected]) => selected)
    .map(([id]) => Number(id))
    .filter((id) => Number.isFinite(id)),
))
// One selection set spans every provider: the backend routes each model on its
// own provider, so a batch may freely mix NVIDIA and OpenCodeFree models.
const selectedTestModelList = computed<Model[]>(() => modelList.value.filter(
  (model) => selectedTestModelIds.value.has(model.id),
))
const candidateSelectedCount = computed(() => selectedCandidateKeys.value.size)
const candidateTestSelectedCount = computed(() => filteredCandidates.value.filter(
  (candidate) => selectedCandidateKeys.value.has(candidateSelectionKey(candidate)),
).length)
const testProgress = computed(() => {
  const job = testJob.value
  if (!job || job.total <= 0) return 0
  return Math.min(100, Math.round((job.completed / job.total) * 100))
})
const testJobActive = computed(() => {
  const status = testJob.value?.status.toLowerCase()
  return status === 'queued' || status === 'running'
})
const canStartBatchTest = computed(() => selectedTestModelList.value.length > 0 && !testJobActive.value && !testJobLoading.value)

watch(modelList, (next) => {
  const previous = selectedTestModels.value
  const nextSelection: Record<string, boolean> = {}
  for (const model of next) {
    const key = String(model.id)
    nextSelection[key] = Object.prototype.hasOwnProperty.call(previous, key)
      ? previous[key] === true
      : model.enabled
  }
  selectedTestModels.value = nextSelection
}, { immediate: true })

onMounted(() => {
  void loadModels()
})

onBeforeUnmount(() => {
  jobPollToken.value += 1
  clearJobPoll()
})

function isModel(value: unknown): value is Model {
  return isRecord(value)
    && isFiniteNumber(value.id)
    && typeof value.public_id === 'string'
    && typeof value.upstream_id === 'string'
    && typeof value.display_name === 'string'
    && typeof value.kind === 'string'
    && typeof value.enabled === 'boolean'
    && typeof value.supports_vision === 'boolean'
    && typeof value.supports_tools === 'boolean'
    && typeof value.supports_reasoning === 'boolean'
    && (value.provider === undefined || typeof value.provider === 'string')
    && (value.capabilities === undefined || isStringArray(value.capabilities))
    && (value.blocked_by_key_ids === undefined
      || (Array.isArray(value.blocked_by_key_ids) && value.blocked_by_key_ids.every(isFiniteNumber)))
    && (value.capability_verified_at === undefined || typeof value.capability_verified_at === 'string')
    && (value.tools_verified_at === undefined || typeof value.tools_verified_at === 'string')
    && (value.stream_first_token_timeout_ms === undefined || typeof value.stream_first_token_timeout_ms === 'number')
    && (value.stream_idle_timeout_ms === undefined || typeof value.stream_idle_timeout_ms === 'number')
    && (value.context_length === undefined || typeof value.context_length === 'number')
    && (value.reasoning_wire_format === undefined || typeof value.reasoning_wire_format === 'string')
    && (value.reasoning_status === undefined || typeof value.reasoning_status === 'string')
}

function isCandidate(value: unknown): value is Candidate {
  return isRecord(value)
    && typeof value.upstream_id === 'string'
    && typeof value.display_name === 'string'
    && typeof value.kind === 'string'
    && typeof value.supports_vision === 'boolean'
    && typeof value.supports_tools === 'boolean'
    && typeof value.supports_reasoning === 'boolean'
    && (value.provider === undefined || typeof value.provider === 'string')
    && (value.channel === undefined || typeof value.channel === 'string')
    && (value.badge === undefined || typeof value.badge === 'string')
    && (value.status === undefined || typeof value.status === 'string')
    && (value.public_id === undefined || typeof value.public_id === 'string')
    && (value.capabilities === undefined || isStringArray(value.capabilities))
    && (value.reasoning_wire_format === undefined || typeof value.reasoning_wire_format === 'string')
    && (value.reasoning_status === undefined || typeof value.reasoning_status === 'string')
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === 'string')
}

function isProbeSummary(value: unknown): boolean {
  return isRecord(value)
    && typeof value.base === 'string'
    && typeof value.reasoning === 'string'
    && typeof value.tools === 'string'
    && (value.reasoning_wire_format === undefined || typeof value.reasoning_wire_format === 'string')
}

function isModelTestResult(value: unknown): value is ModelTestResult {
  return isRecord(value)
    && isFiniteNumber(value.model_id)
    && typeof value.status === 'string'
    && (value.public_id === undefined || typeof value.public_id === 'string')
    && (value.provider === undefined || typeof value.provider === 'string')
    && (value.duration_ms === undefined || isFiniteNumber(value.duration_ms))
    && (value.error === undefined || typeof value.error === 'string')
    && (value.started_at === undefined || typeof value.started_at === 'string')
    && (value.finished_at === undefined || typeof value.finished_at === 'string')
    && (value.probe === undefined || isProbeSummary(value.probe))
}

function readModelTestJob(value: unknown): ModelTestJob | null {
  if (!isRecord(value)) return null
  const nested = isRecord(value.job) ? value.job : isRecord(value.data) ? value.data : value
  const id = nested.id
  if (!(typeof id === 'string' || isFiniteNumber(id))) return null
  const statusValue = nested.status ?? nested.state
  const status = typeof statusValue === 'string' ? statusValue : 'queued'
  const mode: ModelTestMode = nested.mode === 'sequential' ? 'sequential' : 'concurrent'
  const resultValue = Array.isArray(nested.results)
    ? nested.results
    : Array.isArray(nested.items) ? nested.items : []
  const results = resultValue.filter(isModelTestResult)
  const total = isFiniteNumber(nested.total) ? nested.total : results.length
  const completed = isFiniteNumber(nested.completed)
    ? nested.completed
    : results.filter((result) => isTerminalResult(result.status)).length
  return {
    id,
    mode,
    status,
    total,
    completed,
    results,
    error: typeof nested.error === 'string' ? nested.error : undefined,
    created_at: typeof nested.created_at === 'string' ? nested.created_at : undefined,
    started_at: typeof nested.started_at === 'string' ? nested.started_at : undefined,
    finished_at: typeof nested.finished_at === 'string' ? nested.finished_at : undefined,
  }
}

function matchesFilter(item: Candidate | Model): boolean {
  const provider = normalizeProvider(item.provider)
  if (providerFilter.value !== 'all' && provider !== providerFilter.value) return false
  const query = candidateSearch.value.trim().toLowerCase()
  if (!query) return true
  const publicId = 'id' in item ? item.public_id : candidatePublicId(item)
  const values = [
    item.upstream_id,
    publicId,
    item.display_name,
    provider,
    item.kind,
    ...capabilityLabels(item),
  ]
  return values.some((value) => value.toLowerCase().includes(query))
}

function providerLabel(provider?: string): string {
  return normalizeProvider(provider) === 'opencodefree' ? 'OpenCodeFree' : 'NVIDIA'
}

function isTerminalResult(status: string): boolean {
  return ['success', 'succeeded', 'failed', 'error', 'cancelled', 'canceled'].includes(status.toLowerCase())
}

function isTerminalJob(status: string): boolean {
  return ['completed', 'failed', 'cancelled', 'canceled'].includes(status.toLowerCase())
}

function jobStatusLabel(status: string): string {
  switch (status.toLowerCase()) {
    case 'queued': return '排队中'
    case 'running': return '测试中'
    case 'completed': return '已完成'
    case 'failed': return '任务失败'
    case 'cancelled':
    case 'canceled': return '已取消'
    default: return status
  }
}

function probeStatusLabel(status: string): string {
  switch (status) {
    case 'success': return '基础✓'
    case 'visible': return '思考可见'
    case 'hidden': return '思考隐藏'
    case 'unsupported': return '思考不支持'
    case 'supported': return '工具✓'
    case 'unknown': return '待定'
    default: return status
  }
}

function probeSummaryLabel(result: ModelTestResult): string {
  const probe = result.probe
  if (!probe) return '—'
  const parts = [probeStatusLabel(probe.base), probeStatusLabel(probe.reasoning), probeStatusLabel(probe.tools)]
  if (probe.reasoning_wire_format) parts.push(probe.reasoning_wire_format)
  return parts.join(' · ')
}

function resultStatusLabel(status: string): string {
  switch (status.toLowerCase()) {
    case 'success':
    case 'succeeded': return '成功'
    case 'failed':
    case 'error': return '失败'
    case 'running': return '测试中'
    case 'cancelled':
    case 'canceled': return '已取消'
    default: return status
  }
}

function formatDuration(durationMs?: number): string {
  if (durationMs === undefined) return '—'
  if (durationMs < 1000) return `${Math.round(durationMs)} ms`
  return `${(durationMs / 1000).toFixed(1)} s`
}

async function discover(): Promise<void> {
  discovering.value = true
  candidateMessage.value = ''
  errorMessage.value = ''
  try {
    const response: unknown = await modelsApi.candidates()
    if (isDisposed()) return
    if (!isDataArrayResponse(response, isCandidate)) {
      throw new TypeError('Invalid model candidates response.')
    }
    candidates.value = response.data
    const configured = new Set(modelList.value.map((model) => model.public_id))
    const previous = selectedCandidates.value
    selectedCandidates.value = Object.fromEntries(candidates.value.map((candidate) => {
      const key = candidateSelectionKey(candidate)
      return [key, Object.prototype.hasOwnProperty.call(previous, key)
        ? previous[key] === true
        : configured.has(candidatePublicId(candidate))]
    }))
    candidateMessage.value = `发现 ${candidates.value.length} 个候选模型。`
  } catch (error) {
    if (isDisposed()) return
    errorMessage.value = error instanceof ApiError ? error.message : '候选模型发现失败。'
  } finally {
    if (!isDisposed()) discovering.value = false
  }
}

function selectionFor(candidate: Candidate): SaveSelection {
  const selection: SaveSelection = {
    public_id: candidatePublicId(candidate),
    upstream_id: candidate.upstream_id,
    display_name: candidate.display_name,
    kind: candidate.kind,
    enabled: false,
    supports_vision: candidate.supports_vision,
    supports_tools: candidate.supports_tools,
    supports_reasoning: candidate.supports_reasoning,
    reasoning_status: candidate.reasoning_status,
  }
  if (candidate.reasoning_wire_format !== undefined) {
    selection.reasoning_wire_format = candidate.reasoning_wire_format
  }
  if (normalizeProvider(candidate.provider) !== 'nvidia') {
    selection.provider = normalizeProvider(candidate.provider)
  }
  return selection
}

function toggleCandidate(candidate: Candidate, selected: boolean): void {
  selectedCandidates.value = { ...selectedCandidates.value, [candidateSelectionKey(candidate)]: selected }
}

function toggleTestModel(model: Model, selected: boolean): void {
  selectedTestModels.value = { ...selectedTestModels.value, [String(model.id)]: selected }
}

function selectAllCandidates(): void {
  const allSelected = filteredCandidates.value.length > 0
    && filteredCandidates.value.every((candidate) => selectedCandidateKeys.value.has(candidateSelectionKey(candidate)))
  const next = { ...selectedCandidates.value }
  for (const candidate of filteredCandidates.value) next[candidateSelectionKey(candidate)] = !allSelected
  selectedCandidates.value = next
}

function selectAllTestModels(): void {
  const allSelected = filteredModels.value.length > 0
    && filteredModels.value.every((model) => selectedTestModelIds.value.has(model.id))
  const next = { ...selectedTestModels.value }
  for (const model of filteredModels.value) next[String(model.id)] = !allSelected
  selectedTestModels.value = next
}

function selectEnabledTestModels(): void {
  const next = { ...selectedTestModels.value }
  for (const model of filteredModels.value) next[String(model.id)] = model.enabled
  selectedTestModels.value = next
}

async function saveCandidates(): Promise<void> {
  const configured = new Set(modelList.value.map((model) => model.public_id))
  const selected = candidates.value
    .filter((candidate) => selectedCandidates.value[candidateSelectionKey(candidate)]
      && !configured.has(candidatePublicId(candidate)))
    .map(selectionFor)
  saving.value = true
  errorMessage.value = ''
  try {
    await modelsApi.save(selected)
    if (isDisposed()) return
    await loadModels()
    if (isDisposed()) return
    const nextConfigured = new Set(modelList.value.map((model) => model.public_id))
    selectedCandidates.value = Object.fromEntries(candidates.value.map((candidate) => [
      candidateSelectionKey(candidate), nextConfigured.has(candidatePublicId(candidate)),
    ]))
    candidateMessage.value = `已保存 ${selected.length} 个模型。`
  } catch (error) {
    if (isDisposed()) return
    errorMessage.value = error instanceof ApiError ? error.message : '保存模型白名单失败。'
  } finally {
    if (!isDisposed()) saving.value = false
  }
}

async function runCandidateTest(candidate: Candidate): Promise<void> {
  const key = candidateSelectionKey(candidate)
  candidateTestStates.value = { ...candidateTestStates.value, [key]: { status: 'running' } }
  try {
    const result = await modelsApi.testCandidate({
      provider: normalizeProvider(candidate.provider),
      upstream_id: candidate.upstream_id,
    })
    if (isDisposed()) return
    candidateTestStates.value = {
      ...candidateTestStates.value,
      [key]: {
        status: result.status === 'success' ? 'success' : 'failed',
        duration_ms: result.duration_ms,
        error: result.error,
      },
    }
  } catch (error) {
    if (isDisposed()) return
    candidateTestStates.value = {
      ...candidateTestStates.value,
      [key]: {
        status: 'failed',
        error: error instanceof ApiError ? error.message : '候选模型测试失败。',
      },
    }
  }
}

async function testCandidate(candidate: Candidate): Promise<void> {
  if (candidateBatchTesting.value) return
  errorMessage.value = ''
  await runCandidateTest(candidate)
}

// Batch sweep over the selected candidates with a small fixed concurrency: the
// backend caps simultaneous probes at 4, so three workers never hit that gate.
async function testSelectedCandidates(): Promise<void> {
  if (candidateBatchTesting.value) {
    candidateBatchStopRequested.value = true
    return
  }
  const targets = filteredCandidates.value.filter(
    (candidate) => selectedCandidateKeys.value.has(candidateSelectionKey(candidate)),
  )
  if (targets.length === 0) {
    candidateMessage.value = '请先勾选要测试的候选模型。'
    return
  }
  candidateBatchTesting.value = true
  candidateBatchStopRequested.value = false
  errorMessage.value = ''
  candidateMessage.value = `正在测试 ${targets.length} 个候选模型…`
  const queue = [...targets]
  const worker = async (): Promise<void> => {
    while (queue.length > 0) {
      if (candidateBatchStopRequested.value || isDisposed()) return
      const candidate = queue.shift()
      if (!candidate) return
      await runCandidateTest(candidate)
    }
  }
  await Promise.all(Array.from({ length: Math.min(3, queue.length) }, () => worker()))
  if (isDisposed()) return
  candidateBatchTesting.value = false
  const failed = targets.filter((candidate) => {
    const state = candidateTestStates.value[candidateSelectionKey(candidate)]
    return state?.status === 'failed'
  }).length
  candidateMessage.value = candidateBatchStopRequested.value
    ? '候选测试已停止。'
    : `候选测试完成：${targets.length - failed}/${targets.length} 可用。`
}

async function toggleModel(model: Model): Promise<void> {
  busyId.value = model.id
  errorMessage.value = ''
  try {
    const updated: unknown = await modelsApi.patch(model.id, { enabled: !model.enabled })
    if (isDisposed()) return
    if (!isModel(updated)) {
      throw new TypeError('Invalid model patch response.')
    }
    replaceModel(updated)
    toastSuccess(`模型「${updated.display_name}」已${updated.enabled ? '启用' : '停用'}。`)
  } catch (error) {
    if (isDisposed()) return
    errorMessage.value = error instanceof ApiError ? error.message : '更新模型状态失败。'
    toastError(errorMessage.value)
  } finally {
    if (!isDisposed()) busyId.value = null
  }
}

async function unblockModel(keyId: number, model: Model): Promise<void> {
  busyId.value = model.id
  errorMessage.value = ''
  try {
    await modelsApi.unblock(keyId, model.id)
    if (isDisposed()) return
    await loadModels()
    toastSuccess(`模型「${model.display_name}」已解除阻断。`)
  } catch (error) {
    if (isDisposed()) return
    errorMessage.value = error instanceof ApiError ? error.message : '模型 block 恢复失败。'
    toastError(errorMessage.value)
  } finally {
    if (!isDisposed()) busyId.value = null
  }
}

function replaceModel(updated: Model): void {
  const index = modelList.value.findIndex((model) => model.id === updated.id)
  if (index >= 0 && models.value) models.value[index] = updated
}

async function saveContextLength(model: Model, contextLength: number): Promise<void> {
  busyId.value = model.id
  errorMessage.value = ''
  try {
    const updated: unknown = await modelsApi.patch(model.id, { context_length: contextLength })
    if (isDisposed()) return
    if (!isModel(updated)) {
      throw new TypeError('Invalid model patch response.')
    }
    replaceModel(updated)
    toastSuccess(`模型「${updated.display_name}」上下文窗口已更新。`)
  } catch (error) {
    if (isDisposed()) return
    errorMessage.value = error instanceof ApiError ? error.message : '保存上下文窗口失败。'
    toastError(errorMessage.value)
  } finally {
    if (!isDisposed()) busyId.value = null
  }
}

async function confirmDelete(): Promise<void> {
  const model = pendingDelete.value
  if (!model || deleting.value) return
  deleting.value = true
  busyId.value = model.id
  try {
    await modelsApi.delete(model.id)
    if (isDisposed()) return
    pendingDelete.value = null
    if (models.value) models.value = models.value.filter((item) => item.id !== model.id)
    toastSuccess(`模型「${model.display_name}」已从白名单中删除。`)
  } catch (error) {
    if (isDisposed()) return
    errorMessage.value = error instanceof ApiError ? error.message : '删除模型失败。'
    toastError(errorMessage.value)
  } finally {
    if (!isDisposed()) {
      busyId.value = null
      deleting.value = false
    }
  }
}

function normalizedConcurrency(): number {
  const value = Number.isFinite(testConcurrency.value) ? Math.round(testConcurrency.value) : 4
  return Math.min(8, Math.max(2, value))
}

async function startSingleTest(model: Model): Promise<void> {
  toggleTestModel(model, true)
  await startTest([model], 'sequential')
}

async function startBatchTest(): Promise<void> {
  await startTest(selectedTestModelList.value, testMode.value)
}

async function startTest(modelsToTest: Model[], mode: ModelTestMode): Promise<void> {
  if (testJobActive.value || testJobLoading.value) return
  if (modelsToTest.length === 0) {
    testJobError.value = '请先选择要测试的模型。'
    return
  }
  const request: ModelTestJobRequest = {
    model_ids: modelsToTest.map((model) => model.id),
    mode,
    concurrency: mode === 'concurrent' ? normalizedConcurrency() : 1,
  }
  testJobLoading.value = true
  testJobError.value = ''
  clearJobPoll()
  jobPollToken.value += 1
  const token = jobPollToken.value
  try {
    const response: unknown = await modelsApi.createTestJob(request)
    if (isDisposed() || token !== jobPollToken.value) return
    const job = readModelTestJob(response)
    if (!job) throw new TypeError('Invalid model test job response.')
    testJob.value = job
    if (!isTerminalJob(job.status)) scheduleJobPoll(job.id, token)
  } catch (error) {
    if (isDisposed() || token !== jobPollToken.value) return
    testJobError.value = error instanceof ApiError ? error.message : '模型测试任务创建失败。'
  } finally {
    if (!isDisposed() && token === jobPollToken.value) testJobLoading.value = false
  }
}

function clearJobPoll(): void {
  if (jobPollTimer.value !== undefined) {
    globalThis.clearTimeout(jobPollTimer.value)
    jobPollTimer.value = undefined
  }
}

function scheduleJobPoll(id: string | number, token: number): void {
  clearJobPoll()
  jobPollTimer.value = globalThis.setTimeout(() => void pollTestJob(id, token), 500)
}

async function pollTestJob(id: string | number, token: number): Promise<void> {
  if (token !== jobPollToken.value || isDisposed()) return
  try {
    const response: unknown = await modelsApi.getTestJob(id)
    if (isDisposed() || token !== jobPollToken.value) return
    const job = readModelTestJob(response)
    if (!job) throw new TypeError('Invalid model test job status response.')
    testJob.value = job
    if (!isTerminalJob(job.status)) scheduleJobPoll(id, token)
    else clearJobPoll()
  } catch (error) {
    if (isDisposed() || token !== jobPollToken.value) return
    testJobError.value = error instanceof ApiError ? error.message : '模型测试进度读取失败，将继续重试。'
    scheduleJobPoll(id, token)
  }
}

async function cancelCurrentTest(): Promise<void> {
  const job = testJob.value
  if (!job || !testJobActive.value || testJobCancelling.value) return
  testJobCancelling.value = true
  jobPollToken.value += 1
  clearJobPoll()
  try {
    const response: unknown = await modelsApi.cancelTestJob(job.id)
    if (isDisposed()) return
    const cancelled = readModelTestJob(response)
    testJob.value = cancelled ?? {
      ...job,
      status: 'cancelled',
      completed: Math.max(job.completed, job.total),
    }
  } catch (error) {
    if (isDisposed()) return
    testJobError.value = error instanceof ApiError ? error.message : '取消模型测试任务失败。'
    const token = jobPollToken.value
    scheduleJobPoll(job.id, token)
  } finally {
    if (!isDisposed()) testJobCancelling.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div class="content-wrapper">
      <UiPageHeader
        eyebrow="资源接入"
        title="模型白名单"
        subtitle="候选发现、白名单管理和只读模型测试在同一目录中完成。"
      >
        <template #actions>
          <UiButton
            data-testid="discover-models"
            variant="secondary"
            icon="search"
            :loading="discovering"
            loading-label="发现中…"
            @click="discover"
          >
            发现候选模型
          </UiButton>
        </template>
      </UiPageHeader>

      <Transition name="slide">
        <p
          v-if="candidateMessage"
          class="badge-success mb-4 inline-flex px-3 py-1 text-sm"
        >
          {{ candidateMessage }}
        </p>
      </Transition>
      <Transition name="slide">
        <p
          v-if="errorMessage"
          class="mb-4 text-sm text-[var(--color-danger)]"
          role="alert"
        >
          {{ errorMessage }}
        </p>
      </Transition>

      <p
        data-testid="mobile-model-hint"
        class="panel-inset mb-4 px-3 py-2 text-xs text-[var(--color-text-muted)] md:hidden"
      >
        移动端可查看和切换模型状态；批量筛选、保存和测试控制也保留在本页，桌面端操作更完整。
      </p>

      <UiStatePanel
        :loading="loading && modelList.length === 0 && candidates.length === 0"
        :error="loadError"
        :empty="modelList.length === 0 && candidates.length === 0"
        loadingLabel="模型列表加载中…"
        skeleton="table"
        emptyLabel="模型目录暂无数据"
        emptyHint="点击右上角「发现候选模型」从上游拉取可用模型并勾选保存。"
        empty-icon="model"
        errorTestId="models-load-error"
        retryTestId="models-retry"
        @retry="loadModels"
      >
        <UiCard
          data-testid="model-catalog"
          title="模型目录"
          subtitle="搜索会匹配模型 ID、显示名、渠道、类型和能力标签；切换筛选不会清除已选项。"
          class="mb-5"
        >
          <template #actions>
            <UiButton
              data-testid="save-candidates"
              variant="primary"
              :loading="saving"
              loading-label="保存中…"
              :disabled="candidateSelectedCount === 0"
              @click="saveCandidates"
            >
              保存 {{ candidateSelectedCount }} 项
            </UiButton>
          </template>

          <div
            data-testid="model-filter-toolbar"
            class="model-filter-toolbar grid gap-4 border-b border-[var(--color-border-subtle)] pb-5 md:grid-cols-[minmax(0,1fr)_12rem]"
          >
            <label class="block">
              <span class="mb-2 block text-xs font-medium tracking-[0.02em] text-[var(--color-text-secondary)]">搜索模型</span>
              <input
                v-model="candidateSearch"
                data-testid="model-search"
                class="input-field border-[var(--color-border)] bg-[var(--color-surface)] focus:border-[var(--color-border-strong)]"
                type="search"
                autocomplete="off"
                placeholder="模型 ID、显示名、渠道、类型或能力"
              >
            </label>
            <label class="block">
              <span class="mb-2 block text-xs font-medium tracking-[0.02em] text-[var(--color-text-secondary)]">渠道筛选</span>
              <UiSelect
                v-model="providerFilter"
                data-testid="model-provider-filter"
                class="border-[var(--color-border)] bg-[var(--color-surface)] focus:border-[var(--color-border-strong)]"
                aria-label="渠道筛选"
              >
                <option value="all">全部渠道</option>
                <option
                  v-for="provider in providerOptions"
                  :key="provider"
                  :value="provider"
                >
                  {{ providerLabel(provider) }}
                </option>
              </UiSelect>
            </label>
          </div>

          <div
            data-testid="model-selection-toolbar"
            class="model-selection-toolbar mt-4 flex flex-wrap items-center justify-between gap-x-4 gap-y-3"
          >
            <div
              class="flex flex-wrap items-center gap-2"
              role="group"
              aria-label="候选模型批量选择"
            >
              <UiButton
                data-testid="select-all-candidates"
                variant="secondary"
                size="sm"
                :disabled="filteredCandidates.length === 0"
                @click="selectAllCandidates"
              >
                全选当前筛选结果
              </UiButton>
              <UiButton
                data-testid="test-selected-candidates"
                :variant="candidateBatchTesting ? 'danger' : 'secondary'"
                size="sm"
                :disabled="!candidateBatchTesting && candidateTestSelectedCount === 0"
                @click="testSelectedCandidates"
              >
                {{ candidateBatchTesting ? '停止候选测试' : `测试选中候选 ${candidateTestSelectedCount}` }}
              </UiButton>
              <span
                data-testid="model-candidate-count"
                class="whitespace-nowrap text-xs tabular-nums text-[var(--color-text-muted)]"
              >
                候选 {{ candidateSelectedCount }} / {{ candidates.length }}
              </span>
            </div>
            <div
              class="flex flex-wrap items-center gap-2"
              role="group"
              aria-label="测试模型批量选择"
            >
              <UiButton
                data-testid="select-enabled-test-models"
                variant="ghost"
                size="sm"
                :disabled="filteredModels.length === 0"
                @click="selectEnabledTestModels"
              >
                选中启用模型
              </UiButton>
              <UiButton
                data-testid="select-all-test-models"
                variant="ghost"
                size="sm"
                :disabled="filteredModels.length === 0"
                @click="selectAllTestModels"
              >
                全选当前筛选模型
              </UiButton>
              <span
                data-testid="model-test-count"
                class="whitespace-nowrap text-xs tabular-nums text-[var(--color-text-muted)]"
              >
                测试 {{ selectedTestModelList.length }} 项
              </span>
            </div>
          </div>

          <div
            v-if="filteredModels.length === 0 && filteredCandidates.length === 0"
            class="panel-inset mt-4 p-5 text-center text-sm text-[var(--color-text-muted)]"
          >
            当前筛选条件没有匹配的模型。
          </div>
          <template v-else>
            <ModelTable
              :models="filteredModels"
              :candidates="filteredCandidates"
              :busy-id="busyId"
              :selected-model-ids="selectedTestModelIds"
              :selected-candidate-keys="selectedCandidateKeys"
              :candidate-test-states="candidateTestStates"
              :candidate-batch-testing="candidateBatchTesting"
              @toggle="toggleModel"
              @unblock="unblockModel"
              @save-context-length="saveContextLength"
              @delete="pendingDelete = $event"
              @toggle-test="toggleTestModel"
              @toggle-candidate="toggleCandidate"
              @test="startSingleTest"
              @test-candidate="testCandidate"
            />
            <ModelCards
              :models="filteredModels"
              :candidates="filteredCandidates"
              :busy-id="busyId"
              :selected-model-ids="selectedTestModelIds"
              :selected-candidate-keys="selectedCandidateKeys"
              :candidate-test-states="candidateTestStates"
              :candidate-batch-testing="candidateBatchTesting"
              @toggle="toggleModel"
              @unblock="unblockModel"
              @delete="pendingDelete = $event"
              @toggle-test="toggleTestModel"
              @toggle-candidate="toggleCandidate"
              @test="startSingleTest"
              @test-candidate="testCandidate"
            />
          </template>

          <div
            data-testid="model-test-controls"
            class="mt-5 border-t border-[var(--color-border)] pt-5"
          >
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h3 class="text-sm font-semibold text-[var(--color-text)]">
                  只读模型测试
                </h3>
                <p class="mt-1 text-xs text-[var(--color-text-muted)]">
                  一个任务可混选任意渠道的模型：后端按每个模型自己的渠道选路，NVIDIA 随机取可用 Key、OpenCodeFree 换代理出口，失败会自动重试几次。测试不会启用模型、清除 block，也不代表生产调用能力。
                </p>
              </div>
              <UiBadge
                variant="info"
                label="异步任务"
                :dot="false"
              />
            </div>

            <div class="mt-4 grid gap-3 sm:grid-cols-2">
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-[var(--color-text-secondary)]">执行方式</span>
                <UiSelect
                  v-model="testMode"
                  data-testid="model-test-mode"
                  aria-label="执行方式"
                >
                  <option value="concurrent">有界并发</option>
                  <option value="sequential">顺序执行</option>
                </UiSelect>
              </label>
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-[var(--color-text-secondary)]">并发度（2–8）</span>
                <input
                  v-model.number="testConcurrency"
                  data-testid="model-test-concurrency"
                  class="input-field"
                  type="number"
                  min="2"
                  max="8"
                  step="1"
                  :disabled="testMode === 'sequential'"
                >
              </label>
            </div>
            <div class="mt-4 flex flex-wrap items-center gap-3">
              <UiButton
                data-testid="start-model-test"
                variant="primary"
                icon="play"
                :loading="testJobLoading"
                loading-label="创建任务…"
                :disabled="!canStartBatchTest"
                @click="startBatchTest"
              >
                测试 {{ selectedTestModelList.length }} 个模型
              </UiButton>
              <span class="text-xs text-[var(--color-text-muted)]">
                已选 {{ selectedTestModelList.length }} 项
              </span>
            </div>
            <p
              v-if="testJobError"
              data-testid="model-test-error"
              class="mt-3 text-sm text-[var(--color-danger)]"
              role="status"
            >
              {{ testJobError }}
            </p>

            <div
              v-if="testJob"
              data-testid="model-test-job"
              class="panel-inset mt-4 p-4"
            >
              <div class="flex flex-wrap items-center justify-between gap-3">
                <div>
                  <p class="text-sm font-medium text-[var(--color-text)]">
                    测试任务 #{{ testJob.id }} · {{ jobStatusLabel(testJob.status) }}
                  </p>
                  <p class="mt-1 text-xs text-[var(--color-text-muted)]">
                    {{ testJob.mode === 'concurrent' ? `并发 ${testJob.total > 0 ? Math.min(8, Math.max(2, testConcurrency)) : 0}` : '顺序' }} · {{ testJob.completed }} / {{ testJob.total }} 完成
                  </p>
                </div>
                <UiButton
                  v-if="testJobActive"
                  data-testid="cancel-model-test"
                  variant="danger"
                  size="sm"
                  :loading="testJobCancelling"
                  loading-label="取消中…"
                  @click="cancelCurrentTest"
                >
                  取消任务
                </UiButton>
              </div>
              <div
                class="mt-3 h-2 overflow-hidden rounded-full bg-[var(--color-border)]"
                role="progressbar"
                aria-label="模型测试进度"
                :aria-valuenow="testProgress"
                aria-valuemin="0"
                aria-valuemax="100"
              >
                <div
                  class="h-full rounded-full bg-[var(--color-accent)] transition-[width]"
                  :style="{ width: `${testProgress}%` }"
                />
              </div>
              <div class="mt-4 overflow-x-auto">
                <table class="data-table min-w-[580px]">
                  <thead>
                    <tr>
                      <th
                        class="data-table-th"
                        scope="col"
                      >
                        模型
                      </th>
                      <th
                        class="data-table-th"
                        scope="col"
                      >
                        渠道
                      </th>
                      <th
                        class="data-table-th"
                        scope="col"
                      >
                        状态
                      </th>
                      <th
                        class="data-table-th"
                        scope="col"
                      >
                        探测摘要
                      </th>
                      <th
                        class="data-table-th"
                        scope="col"
                      >
                        耗时
                      </th>
                      <th
                        class="data-table-th"
                        scope="col"
                      >
                        错误
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr
                      v-for="result in testJob.results"
                      :key="`${testJob.id}-${result.model_id}`"
                      :data-testid="`model-test-result-${result.model_id}`"
                      class="data-table-row"
                    >
                      <td class="data-table-td font-mono-data text-xs">
                        {{ result.public_id || modelList.find((model) => model.id === result.model_id)?.public_id || `#${result.model_id}` }}
                      </td>
                      <td class="data-table-td text-xs text-[var(--color-text-secondary)]">
                        {{ providerLabel(result.provider) }}
                      </td>
                      <td class="data-table-td text-xs">
                        <UiBadge
                          :variant="result.status.toLowerCase() === 'success' || result.status.toLowerCase() === 'succeeded' ? 'success' : result.status.toLowerCase() === 'failed' || result.status.toLowerCase() === 'error' ? 'danger' : 'muted'"
                          :label="resultStatusLabel(result.status)"
                          :dot="false"
                        />
                      </td>
                      <td class="data-table-td max-w-[360px] text-xs text-[var(--color-text-secondary)]">
                        {{ probeSummaryLabel(result) }}
                      </td>
                      <td class="data-table-td font-mono-data text-xs">
                        {{ formatDuration(result.duration_ms) }}
                      </td>
                      <td class="data-table-td max-w-[320px] text-xs text-[var(--color-danger)]">
                        {{ result.error || '—' }}
                      </td>
                    </tr>
                    <tr v-if="testJob.results.length === 0">
                      <td
                        class="data-table-td text-center text-xs text-[var(--color-text-muted)]"
                        colspan="6"
                      >
                        任务已创建，等待逐项结果…
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        </UiCard>
      </UiStatePanel>
    </div>

    <UiConfirmDialog
      :open="pendingDelete !== null"
      title="删除模型"
      :message="pendingDelete ? `将「${pendingDelete.display_name}」从白名单中删除。删除后客户端无法再按公开 ID 调用该模型。` : ''"
      confirm-label="删除"
      :busy="deleting"
      confirm-test-id="confirm-delete-model"
      cancel-test-id="cancel-delete-model"
      @confirm="confirmDelete"
      @cancel="pendingDelete = null"
    />
  </div>
</template>

<style scoped>
.slide-enter-active {
  transition: opacity 0.25s cubic-bezier(0.0, 0.0, 0.2, 1), transform 0.25s cubic-bezier(0.0, 0.0, 0.2, 1);
}
.slide-leave-active {
  transition: opacity 0.18s cubic-bezier(0.4, 0.0, 1, 1), transform 0.18s cubic-bezier(0.4, 0.0, 1, 1);
}
.slide-enter-from,
.slide-leave-to {
  opacity: 0;
  transform: translateY(-8px);
}
</style>
