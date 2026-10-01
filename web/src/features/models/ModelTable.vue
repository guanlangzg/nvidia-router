<script setup lang="ts">
import { ref } from 'vue'

import UiBadge from '../../shared/ui/UiBadge.vue'
import UiButton from '../../shared/ui/UiButton.vue'
import {
  candidatePublicId,
  candidateSelectionKey,
  candidateTestSummary,
  capabilityLabels,
  normalizeProvider,
  toolsStatusLabel,
} from './types'
import type { Candidate, CandidateTestState, Model } from './types'

withDefaults(defineProps<{
  models: Model[]
  candidates?: Candidate[]
  busyId: number | null
  selectedModelIds?: ReadonlySet<number>
  selectedCandidateKeys?: ReadonlySet<string>
  candidateTestStates?: Record<string, CandidateTestState>
  candidateBatchTesting?: boolean
}>(), {
  candidates: () => [],
  selectedModelIds: () => new Set<number>(),
  selectedCandidateKeys: () => new Set<string>(),
  candidateTestStates: () => ({}),
  candidateBatchTesting: false,
})
const emit = defineEmits<{
  toggle: [model: Model]
  unblock: [keyId: number, model: Model]
  saveContextLength: [model: Model, contextLength: number]
  delete: [model: Model]
  toggleTest: [model: Model, selected: boolean]
  toggleCandidate: [candidate: Candidate, selected: boolean]
  test: [model: Model]
  testCandidate: [candidate: Candidate]
}>()

// Same inline-edit pattern for the operator-owned context window declaration.
const editingContext = ref<number | null>(null)
const contextDraft = ref('')

function beginContextEdit(model: Model): void {
  editingContext.value = model.id
  contextDraft.value = model.context_length !== undefined && model.context_length > 0 ? String(model.context_length) : ''
}

function cancelContextEdit(): void {
  editingContext.value = null
}

function submitContextEdit(model: Model): void {
  const trimmed = contextDraft.value.trim()
  if (trimmed === '') {
    emit('saveContextLength', model, 0)
    editingContext.value = null
    return
  }
  const value = Number(trimmed)
  if (!Number.isInteger(value) || value <= 0) return
  emit('saveContextLength', model, value)
  editingContext.value = null
}

// formatStreamTimeout renders the per-model streaming timeout override, or the
// global-default marker when the model carries no override. The columns are
// seeded by migration 016/022 (e.g. deepseek 300s); exposing them here makes the
// override observable without the operator querying the raw API.
// formatContextLength renders the declared context window in tokens; an
// undeclared model shows the explicit marker instead of a misleading 0.
function formatContextLength(value?: number): string {
  if (value === undefined || value <= 0) return '未声明'
  return String(value)
}

function formatStreamTimeout(firstToken?: number, idle?: number): string {
  if (firstToken === undefined && idle === undefined) return '全局默认'
  const parts: string[] = []
  if (firstToken !== undefined) parts.push(`首 ${(firstToken / 1000).toFixed(0)}s`)
  if (idle !== undefined) parts.push(`空闲 ${(idle / 1000).toFixed(0)}s`)
  return parts.join(' · ')
}

function audioNeedsVerification(model: Model): boolean {
  return (model.kind === 'asr' || model.kind === 'tts') && !model.capability_verified_at
}

function enablingIsBlocked(model: Model): boolean {
  return !model.enabled && audioNeedsVerification(model)
}

function providerLabel(provider?: string): string {
  return normalizeProvider(provider) === 'opencodefree' ? 'OpenCodeFree' : 'NVIDIA'
}

function candidateStatus(candidate: Candidate, configured: boolean): string {
  if (candidate.reasoning_status === 'unknown') return '待验证'
  return configured ? '已在白名单' : '候选'
}

function reasoningStatusLabel(status?: string): string {
  switch (status) {
    case 'visible': return '可见'
    case 'hidden': return '隐藏'
    case 'inferred': return '推断'
    case 'unsupported': return '不支持'
    default: return '待验证'
  }
}

function reasoningStatusVariant(status?: string): 'success' | 'warning' | 'muted' {
  if (status === 'visible' || status === 'hidden' || status === 'inferred') return 'success'
  if (status === 'unsupported') return 'muted'
  return 'warning'
}

function onCandidateChange(candidate: Candidate, event: globalThis.Event): void {
  emit('toggleCandidate', candidate, (event.target as globalThis.HTMLInputElement).checked)
}

function onModelTestChange(model: Model, event: globalThis.Event): void {
  emit('toggleTest', model, (event.target as globalThis.HTMLInputElement).checked)
}
</script>

<template>
  <div
    data-testid="model-table"
    class="card hidden overflow-hidden md:block"
  >
    <div
      class="overflow-x-auto"
      tabindex="0"
      role="region"
      aria-label="模型白名单表，可横向滚动"
    >
      <table class="data-table table-fixed min-w-[66rem]">
        <caption class="sr-only">
          模型白名单，共 {{ models.length }} 条
        </caption>
        <thead>
          <tr>
            <th
              class="data-table-th w-24"
              scope="col"
            >
              测试/保存
            </th>
            <th
              class="data-table-th w-56"
              scope="col"
            >
              模型
            </th>
            <th
              class="data-table-th w-24"
              scope="col"
            >
              渠道
            </th>
            <th
              class="data-table-th w-24"
              scope="col"
            >
              Kind
            </th>
            <th
              class="data-table-th w-40"
              scope="col"
            >
              能力
            </th>
            <th
              class="data-table-th w-20"
              scope="col"
            >
              上下文
            </th>
            <th
              class="data-table-th w-22"
              scope="col"
            >
              流式超时
            </th>
            <th
              class="data-table-th w-22"
              scope="col"
            >
              状态
            </th>
            <th
              class="data-table-th w-38 text-right"
              scope="col"
            >
              操作
            </th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="candidate in candidates"
            :key="`candidate-${candidateSelectionKey(candidate)}`"
            class="data-table-row bg-[var(--color-sunken)]"
          >
            <td class="data-table-td">
              <label class="checkbox-row whitespace-nowrap text-xs text-[var(--color-text-muted)]">
                <input
                  :checked="selectedCandidateKeys.has(candidateSelectionKey(candidate))"
                  class="checkbox-control"
                  :data-testid="`candidate-table-${candidateSelectionKey(candidate)}`"
                  type="checkbox"
                  :aria-label="`保存候选模型 ${candidate.display_name}`"
                  @change="onCandidateChange(candidate, $event)"
                >
                保存
              </label>
            </td>
            <td class="data-table-td">
              <p
                class="truncate font-medium text-[var(--color-text)]"
                :title="candidate.display_name"
              >
                {{ candidate.display_name }}
              </p>
              <p
                class="mt-0.5 truncate font-mono-data text-xs text-[var(--color-text-muted)]"
                :title="candidatePublicId(candidate)"
              >
                {{ candidatePublicId(candidate) }}
              </p>
              <p
                v-if="candidatePublicId(candidate) !== candidate.upstream_id"
                class="mt-0.5 truncate text-xs text-[var(--color-text-subtle)]"
                :title="candidate.upstream_id"
              >
                上游 {{ candidate.upstream_id }}
              </p>
            </td>
            <td class="data-table-td">
              <UiBadge
                :variant="candidate.reasoning_status === 'unknown' ? 'warning' : 'info'"
                :label="providerLabel(candidate.provider)"
              />
            </td>
            <td class="data-table-td">
              <UiBadge
                variant="info"
                :label="candidate.kind"
                :dot="false"
              />
            </td>
            <td class="data-table-td">
              <div class="flex flex-wrap gap-x-2 gap-y-1 text-xs">
                <UiBadge
                  v-for="label in capabilityLabels(candidate)"
                  :key="label"
                  variant="success"
                  :label="label"
                  :dot="false"
                />
                <span
                  v-if="capabilityLabels(candidate).length === 0"
                  class="text-xs text-[var(--color-text-subtle)]"
                >
                  —
                </span>
              </div>
            </td>
            <td class="data-table-td text-xs text-[var(--color-text-subtle)]">
              —
            </td>
            <td class="data-table-td text-xs text-[var(--color-text-subtle)]">
              —
            </td>
            <td class="data-table-td text-xs text-[var(--color-text-subtle)]">
              —
            </td>
            <td class="data-table-td">
              <UiBadge
                :variant="candidate.reasoning_status === 'unknown' ? 'warning' : 'muted'"
                :label="candidateStatus(candidate, selectedCandidateKeys.has(candidateSelectionKey(candidate)))"
              />
              <UiBadge
                v-if="candidateTestStates[candidateSelectionKey(candidate)]?.status === 'running'"
                class="mt-1.5"
                variant="info"
                label="测试中…"
                :dot="false"
              />
              <UiBadge
                v-else-if="candidateTestStates[candidateSelectionKey(candidate)]"
                class="mt-1.5"
                :variant="candidateTestStates[candidateSelectionKey(candidate)]?.status === 'success' ? 'success' : 'danger'"
                :label="candidateTestSummary(candidateTestStates[candidateSelectionKey(candidate)])"
                :dot="false"
              />
              <p class="mt-1.5 text-xs text-[var(--color-text-muted)]">
                保存后保持停用，可参与只读测试
              </p>
            </td>
            <td class="data-table-td text-right">
              <UiButton
                :data-testid="`candidate-test-button-${candidateSelectionKey(candidate)}`"
                variant="ghost"
                size="sm"
                :loading="candidateTestStates[candidateSelectionKey(candidate)]?.status === 'running'"
                loading-label="测试中…"
                :disabled="candidateBatchTesting"
                @click="emit('testCandidate', candidate)"
              >
                测试
              </UiButton>
            </td>
          </tr>
          <tr
            v-for="model in models"
            :key="model.id"
            class="data-table-row"
          >
            <td class="data-table-td">
              <label class="checkbox-row whitespace-nowrap text-xs text-[var(--color-text-muted)]">
                <input
                  :checked="selectedModelIds.has(model.id)"
                  class="checkbox-control"
                  :data-testid="`test-model-${model.id}`"
                  type="checkbox"
                  :aria-label="`将模型 ${model.display_name} 加入测试`"
                  @change="onModelTestChange(model, $event)"
                >
                测试
              </label>
            </td>
            <td class="data-table-td">
              <p
                class="truncate font-medium text-[var(--color-text)]"
                :title="model.display_name"
              >
                {{ model.display_name }}
              </p>
              <p
                class="mt-0.5 truncate font-mono-data text-xs text-[var(--color-text-muted)]"
                :title="model.public_id"
              >
                {{ model.public_id }}
              </p>
            </td>
            <td class="data-table-td">
              <UiBadge
                :variant="'info'"
                :label="providerLabel(model.provider)"
              />
            </td>
            <td class="data-table-td">
              <UiBadge
                variant="info"
                :label="model.kind"
                :dot="false"
              />
            </td>
            <td class="data-table-td">
              <div class="flex flex-wrap gap-x-2 gap-y-1 text-xs">
                <UiBadge
                  :variant="model.supports_vision ? 'success' : 'muted'"
                  :label="`Vision ${model.supports_vision ? '✓' : '—'}`"
                  :dot="false"
                />
                <UiBadge
                  :variant="model.tools_status === 'supported' ? 'success' : 'muted'"
                  :label="toolsStatusLabel(model.tools_status)"
                  :dot="false"
                />
                <UiBadge
                  :variant="reasoningStatusVariant(model.reasoning_status)"
                  :label="`Reasoning ${model.supports_reasoning ? '✓' : '—'} · ${reasoningStatusLabel(model.reasoning_status)}`"
                  :dot="false"
                />
              </div>
            </td>
            <td class="data-table-td">
              <div
                v-if="editingContext === model.id"
                :data-testid="`model-context-edit-${model.id}`"
              >
                <input
                  :value="contextDraft"
                  class="input-field h-8 w-24 px-2 text-xs"
                  type="number"
                  min="0"
                  step="1"
                  :data-testid="`model-context-input-${model.id}`"
                  placeholder="tokens"
                  @input="(e: Event) => { contextDraft = (e.target as HTMLInputElement).value }"
                  @keyup.enter="submitContextEdit(model)"
                  @keyup.esc="cancelContextEdit"
                >
                <div class="mt-1.5 flex items-center gap-1.5">
                  <UiButton
                    variant="primary"
                    size="sm"
                    :data-testid="`model-save-context-${model.id}`"
                    :loading="busyId === model.id"
                    loading-label="保存中…"
                    @click="submitContextEdit(model)"
                  >
                    保存
                  </UiButton>
                  <UiButton
                    variant="ghost"
                    size="sm"
                    @click="cancelContextEdit"
                  >
                    取消
                  </UiButton>
                </div>
              </div>
              <button
                v-else
                class="rounded-[var(--radius-control)] px-2 py-1 text-left font-mono-data text-xs transition-colors hover:bg-[var(--color-hover)]"
                :class="model.context_length !== undefined && model.context_length > 0 ? 'text-[var(--color-text-secondary)]' : 'text-[var(--color-text-subtle)]'"
                type="button"
                data-testid="model-edit-context"
                title="点击编辑上下文窗口（tokens），留空表示未声明"
                @click="beginContextEdit(model)"
              >
                {{ formatContextLength(model.context_length) }}
              </button>
            </td>
            <td class="data-table-td">
              <span
                class="font-mono-data text-xs"
                :class="model.stream_first_token_timeout_ms !== undefined || model.stream_idle_timeout_ms !== undefined ? 'text-[var(--color-text)]' : 'text-[var(--color-text-muted)]'"
              >
                {{ formatStreamTimeout(model.stream_first_token_timeout_ms, model.stream_idle_timeout_ms) }}
              </span>
            </td>
            <td class="data-table-td">
              <UiBadge
                :variant="model.enabled ? 'success' : 'muted'"
                :label="model.enabled ? '启用' : '停用'"
              />
              <p
                v-if="model.capability_verified_at"
                class="mt-1.5 text-xs text-[var(--color-text-muted)]"
              >
                已验证
              </p>
              <p
                v-else-if="audioNeedsVerification(model)"
                class="mt-1.5 text-xs text-[var(--color-warning)]"
              >
                需要先完成真实音频能力测试
              </p>
              <div
                v-if="model.blocked_by_key_ids?.length"
                class="mt-2 space-y-1"
              >
                <p class="text-xs text-[var(--color-warning)]">
                  已 block：
                </p>
                <button
                  v-for="keyId in model.blocked_by_key_ids"
                  :key="keyId"
                  :data-testid="`model-table-unblock-${keyId}`"
                  class="block text-xs text-[var(--color-danger)] underline hover:opacity-75 disabled:text-[var(--color-text-subtle)]"
                  type="button"
                  :disabled="busyId === model.id"
                  @click="emit('unblock', keyId, model)"
                >
                  Key #{{ keyId }} · 手测恢复
                </button>
              </div>
            </td>
            <td class="data-table-td">
              <div class="flex justify-end gap-1.5">
                <UiButton
                  :data-testid="`test-model-button-${model.id}`"
                  variant="ghost"
                  size="sm"
                  :disabled="busyId === model.id"
                  @click="emit('test', model)"
                >
                  单测
                </UiButton>
                <UiButton
                  data-testid="model-enable"
                  variant="secondary"
                  size="sm"
                  :disabled="enablingIsBlocked(model) || busyId === model.id"
                  @click="emit('toggle', model)"
                >
                  {{ model.enabled ? '停用' : '启用' }}
                </UiButton>
                <UiButton
                  :data-testid="`model-delete-${model.id}`"
                  variant="danger"
                  size="sm"
                  :disabled="busyId === model.id"
                  @click="emit('delete', model)"
                >
                  删除
                </UiButton>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
