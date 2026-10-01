<script setup lang="ts">
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
  delete: [model: Model]
  toggleTest: [model: Model, selected: boolean]
  toggleCandidate: [candidate: Candidate, selected: boolean]
  test: [model: Model]
  testCandidate: [candidate: Candidate]
}>()

function audioNeedsVerification(model: Model): boolean {
  return (model.kind === 'asr' || model.kind === 'tts') && !model.capability_verified_at
}

function enablingIsBlocked(model: Model): boolean {
  return !model.enabled && audioNeedsVerification(model)
}

function providerLabel(provider?: string): string {
  return normalizeProvider(provider) === 'opencodefree' ? 'OpenCodeFree' : 'NVIDIA'
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
    data-testid="model-cards"
    class="space-y-3 md:hidden"
  >
    <article
      v-for="candidate in candidates"
      :key="`candidate-${candidateSelectionKey(candidate)}`"
      class="card border-dashed p-4"
    >
      <div class="flex items-start gap-3">
        <label class="checkbox-hit shrink-0">
          <input
            :checked="selectedCandidateKeys.has(candidateSelectionKey(candidate))"
            class="checkbox-control"
            :data-testid="`candidate-${candidateSelectionKey(candidate)}`"
            type="checkbox"
            :aria-label="`保存候选模型 ${candidate.display_name}`"
            @change="onCandidateChange(candidate, $event)"
          >
        </label>
        <div class="min-w-0 flex-1">
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <h3 class="text-sm font-medium text-[var(--color-text)]">
                {{ candidate.display_name }}
              </h3>
              <p class="mt-0.5 truncate font-mono-data text-xs text-[var(--color-text-muted)]">
                {{ candidatePublicId(candidate) }}
              </p>
            </div>
            <UiBadge
              class="shrink-0"
              :variant="candidate.reasoning_status === 'unknown' ? 'warning' : 'info'"
              :label="providerLabel(candidate.provider)"
            />
          </div>
          <div class="mt-2 flex flex-wrap gap-1.5 text-xs">
            <UiBadge
              variant="info"
              :label="candidate.kind"
              :dot="false"
            />
            <UiBadge
              v-for="label in capabilityLabels(candidate)"
              :key="label"
              variant="success"
              :label="label"
              :dot="false"
            />
            <UiBadge
              v-if="candidate.reasoning_status === 'unknown'"
              variant="warning"
              label="待验证"
              :dot="false"
            />
            <UiBadge
              v-if="candidateTestStates[candidateSelectionKey(candidate)]?.status === 'running'"
              variant="info"
              label="测试中…"
              :dot="false"
            />
            <UiBadge
              v-else-if="candidateTestStates[candidateSelectionKey(candidate)]"
              :variant="candidateTestStates[candidateSelectionKey(candidate)]?.status === 'success' ? 'success' : 'danger'"
              :label="candidateTestSummary(candidateTestStates[candidateSelectionKey(candidate)])"
              :dot="false"
            />
          </div>
          <div class="mt-3 flex items-center justify-between gap-2">
            <p class="min-w-0 truncate text-xs text-[var(--color-text-muted)]">
              发现候选 · 保存后保持停用，可参与只读测试
            </p>
            <UiButton
              :data-testid="`candidate-test-button-card-${candidateSelectionKey(candidate)}`"
              variant="ghost"
              size="sm"
              class="shrink-0"
              :loading="candidateTestStates[candidateSelectionKey(candidate)]?.status === 'running'"
              loading-label="测试中…"
              :disabled="candidateBatchTesting"
              @click="emit('testCandidate', candidate)"
            >
              测试
            </UiButton>
          </div>
        </div>
      </div>
    </article>
    <article
      v-for="model in models"
      :key="model.id"
      class="card p-4"
    >
      <div class="flex items-start justify-between gap-3">
        <div class="flex min-w-0 items-start gap-2">
          <label class="checkbox-hit shrink-0">
            <input
              :checked="selectedModelIds.has(model.id)"
              class="checkbox-control"
              :data-testid="`test-model-card-${model.id}`"
              type="checkbox"
              :aria-label="`选中测试模型 ${model.display_name}`"
              @change="onModelTestChange(model, $event)"
            >
          </label>
          <div class="min-w-0">
            <h3 class="text-sm font-medium text-[var(--color-text)]">
              {{ model.display_name }}
            </h3>
            <p class="mt-0.5 truncate font-mono-data text-xs text-[var(--color-text-muted)]">
              {{ model.public_id }}
            </p>
          </div>
        </div>
        <UiBadge
          class="shrink-0"
          :variant="'info'"
          :label="providerLabel(model.provider)"
          :dot="false"
        />
      </div>

      <div class="mt-3 flex flex-wrap gap-1.5 text-xs">
        <UiBadge
          variant="info"
          :label="model.kind"
          :dot="false"
        />
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

      <div class="mt-3 flex items-center justify-between">
        <UiBadge
          :variant="model.enabled ? 'success' : 'muted'"
          :label="model.enabled ? '启用' : '停用'"
        />
        <span
          v-if="model.context_length !== undefined && model.context_length > 0"
          class="font-mono-data text-xs text-[var(--color-text-muted)]"
          :data-testid="`model-card-context-${model.id}`"
        >
          上下文 {{ model.context_length }} tokens
        </span>
        <span
          v-else
          class="text-xs text-[var(--color-text-muted)]"
        >勾选后参与只读测试</span>
      </div>

      <p
        v-if="model.capability_verified_at"
        class="mt-2 text-xs text-[var(--color-text-muted)]"
      >
        已验证
      </p>
      <p
        v-else-if="audioNeedsVerification(model)"
        class="mt-2 text-xs text-[var(--color-warning)]"
      >
        需要先完成真实音频能力测试
      </p>

      <div
        v-if="model.blocked_by_key_ids?.length"
        class="mt-3 space-y-1"
      >
        <p class="text-xs text-[var(--color-warning)]">
          已 block：
        </p>
        <button
          v-for="keyId in model.blocked_by_key_ids"
          :key="keyId"
          :data-testid="`model-unblock-${keyId}`"
          class="block text-xs text-[var(--color-danger)] underline disabled:text-[var(--color-text-subtle)]"
          type="button"
          :disabled="busyId === model.id"
          @click="emit('unblock', keyId, model)"
        >
          Key #{{ keyId }} · 手测恢复
        </button>
      </div>

      <div class="mt-4 flex gap-2">
        <UiButton
          :data-testid="`test-model-button-card-${model.id}`"
          variant="ghost"
          size="sm"
          :disabled="busyId === model.id"
          @click="emit('test', model)"
        >
          单测
        </UiButton>
        <UiButton
          data-testid="model-card-toggle"
          variant="secondary"
          size="sm"
          class="flex-1"
          :disabled="enablingIsBlocked(model) || busyId === model.id"
          @click="emit('toggle', model)"
        >
          {{ model.enabled ? '停用' : '启用' }}
        </UiButton>
        <UiButton
          :data-testid="`model-card-delete-${model.id}`"
          variant="danger"
          size="sm"
          :disabled="busyId === model.id"
          @click="emit('delete', model)"
        >
          删除
        </UiButton>
      </div>
    </article>
  </div>
</template>
