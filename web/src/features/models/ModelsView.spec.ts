import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { modelsApi } from './api'
import ModelsView from './ModelsView.vue'
import type { Model, ModelsResponse } from './types'

vi.mock('./api', () => ({
  modelsApi: {
    list: vi.fn(),
    candidates: vi.fn(),
    save: vi.fn(),
    patch: vi.fn(),
    unblock: vi.fn(),
    delete: vi.fn(),
    createTestJob: vi.fn(),
    getTestJob: vi.fn(),
    cancelTestJob: vi.fn(),
    testCandidate: vi.fn(),
  },
}))

function makeModel(overrides: Partial<Model> = {}): Model {
  return {
    id: 1,
    public_id: 'chat-model',
    upstream_id: 'vendor/chat',
    display_name: 'Chat',
    kind: 'chat',
    enabled: true,
    supports_vision: true,
    supports_tools: true,
    supports_reasoning: true,
    ...overrides,
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise
  })
  return { promise, resolve }
}

beforeEach(() => {
  vi.resetAllMocks()
  vi.mocked(modelsApi.list).mockResolvedValue({ data: [] })
})

afterEach(() => {
  document.body.innerHTML = ''
})

function bodyElement<T extends Element>(selector: string): T {
  const element = document.body.querySelector<T>(selector)
  if (!element) throw new Error(`Expected body element: ${selector}`)
  return element
}

describe('ModelsView', () => {
  it.each([
    ['a non-array data field', { data: null }],
    ['a non-numeric model id', { data: [makeModel({ id: null as never })] }],
  ])('shows a visible error for %s in a successful response', async (_name, response) => {
    vi.mocked(modelsApi.list).mockResolvedValue(response as never)
    const wrapper = mount(ModelsView)
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toContain('模型列表加载失败')
    expect(wrapper.text()).not.toContain('Chat')
  })

  it('surfaces a persistent load error with retry instead of an empty model table', async () => {
    vi.mocked(modelsApi.list)
      .mockRejectedValueOnce(new Error('gateway timeout'))
      .mockResolvedValueOnce({ data: [makeModel()] })
    const wrapper = mount(ModelsView)
    await flushPromises()

    const panel = wrapper.get('[data-testid="models-load-error"]')
    expect(panel.text()).toContain('加载失败')

    await wrapper.get('[data-testid="models-retry"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="models-load-error"]').exists()).toBe(false)
    expect(wrapper.text()).toContain(makeModel().display_name)
  })

  it.each([
    ['a non-array data field', { data: null }],
    ['an invalid candidate item', {
      data: [{
        upstream_id: 'vendor/invalid',
        display_name: 'Invalid',
        kind: 'chat',
        supports_vision: false,
        supports_tools: null,
        supports_reasoning: false,
      }],
    }],
  ])('shows a visible error for %s in a candidate response', async (_name, response) => {
    vi.mocked(modelsApi.candidates).mockResolvedValue(response as never)
    const wrapper = mount(ModelsView)
    await flushPromises()

    await wrapper.get('[data-testid="discover-models"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toContain('候选模型发现失败')
    expect(wrapper.find('[data-testid="save-candidates"]').exists()).toBe(false)
  })

  it('keeps the newest list when an older request resolves last', async () => {
    const first = deferred<ModelsResponse>()
    const second = deferred<ModelsResponse>()
    vi.mocked(modelsApi.list)
      .mockReset()
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise)
    vi.mocked(modelsApi.candidates).mockResolvedValue({
      data: [{
        upstream_id: 'vendor/new',
        display_name: 'New candidate',
        kind: 'chat',
        supports_vision: false,
        supports_tools: false,
        supports_reasoning: false,
      }],
    })
    vi.mocked(modelsApi.save).mockResolvedValue({ saved: 1 })
    const wrapper = mount(ModelsView)

    await wrapper.get('[data-testid="discover-models"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="candidate-vendor/new"]').setValue(true)
    await wrapper.get('[data-testid="save-candidates"]').trigger('click')
    expect(modelsApi.list).toHaveBeenCalledTimes(2)

    second.resolve({ data: [makeModel({ id: 2, display_name: '新数据' })] })
    await flushPromises()
    first.resolve({ data: [makeModel({ display_name: '旧数据' })] })
    await flushPromises()

    expect(wrapper.text()).toContain('新数据')
    expect(wrapper.text()).not.toContain('旧数据')
  })

  it('does not update list state after unmount', async () => {
    const request = deferred<ModelsResponse>()
    vi.mocked(modelsApi.list).mockReset().mockReturnValueOnce(request.promise)
    const wrapper = mount(ModelsView)
    const state = wrapper.vm as unknown as { models: Model[]; loading: boolean }

    wrapper.unmount()
    request.resolve({ data: [makeModel()] })
    await flushPromises()

    expect(state.models).toEqual(null)
    expect(state.loading).toBe(true)
  })

  it('does not start a post-save reload after unmount', async () => {
    const save = deferred<{ saved: number }>()
    vi.mocked(modelsApi.candidates).mockResolvedValue({
      data: [{
        upstream_id: 'vendor/late',
        display_name: 'Late candidate',
        kind: 'chat',
        supports_vision: false,
        supports_tools: false,
        supports_reasoning: false,
      }],
    })
    vi.mocked(modelsApi.save).mockReturnValueOnce(save.promise)
    const wrapper = mount(ModelsView)
    await flushPromises()

    await wrapper.get('[data-testid="discover-models"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="candidate-vendor/late"]').setValue(true)
    await wrapper.get('[data-testid="save-candidates"]').trigger('click')
    wrapper.unmount()
    save.resolve({ saved: 1 })
    await flushPromises()

    expect(modelsApi.list).toHaveBeenCalledOnce()
  })

  it('renders desktop table and mobile cards with kind and capability flags', async () => {
    vi.mocked(modelsApi.list).mockResolvedValue({
      data: [makeModel({ kind: 'tts', enabled: false, supports_vision: false })],
    })
    const wrapper = mount(ModelsView)
    await flushPromises()

    const table = wrapper.get('[data-testid="model-table"]')
    const cards = wrapper.get('[data-testid="model-cards"]')
    expect(table.classes()).toEqual(expect.arrayContaining(['hidden', 'md:block']))
    expect(cards.classes()).toEqual(expect.arrayContaining(['md:hidden']))
    expect(table.text()).toContain('tts')
    expect(table.text()).not.toContain('单价')
    expect(wrapper.find('[data-testid="model-edit-price"]').exists()).toBe(false)
    expect(cards.text()).toContain('Vision')
    expect(cards.text()).toContain('Tools')
    expect(cards.text()).toContain('Reasoning')
    expect(wrapper.get('[data-testid="mobile-model-hint"]').text()).toContain('桌面端')
  })

  it('keeps model filters and batch actions in separate toolbars', async () => {
    vi.mocked(modelsApi.list).mockResolvedValue({
      data: [makeModel(), makeModel({ id: 2, public_id: 'free-model', provider: 'opencodefree' })],
    })
    const wrapper = mount(ModelsView)
    await flushPromises()

    expect(wrapper.get('[data-testid="model-filter-toolbar"]').classes()).toEqual(
      expect.arrayContaining(['model-filter-toolbar']),
    )
    expect(wrapper.get('[data-testid="model-selection-toolbar"]').classes()).toEqual(
      expect.arrayContaining(['model-selection-toolbar']),
    )
    expect(wrapper.get('[data-testid="model-candidate-count"]').text()).toContain('候选')
    expect(wrapper.get('[data-testid="model-test-count"]').text()).toContain('测试')
  })

  it('blocks enabling unverified ASR and TTS but allows verified audio models', async () => {
    vi.mocked(modelsApi.list).mockResolvedValue({
      data: [
        makeModel({ id: 2, public_id: 'asr-model', kind: 'asr', enabled: false }),
        makeModel({ id: 3, public_id: 'tts-model', kind: 'tts', enabled: false }),
        makeModel({
          id: 4,
          public_id: 'verified-tts',
          kind: 'tts',
          enabled: false,
          capability_verified_at: '2026-07-30T00:00:00Z',
        }),
      ],
    })
    vi.mocked(modelsApi.patch).mockImplementation(async (id, patch) => makeModel({
      id,
      public_id: id === 4 ? 'verified-tts' : 'audio-model',
      kind: id === 2 ? 'asr' : 'tts',
      enabled: patch.enabled ?? false,
      capability_verified_at: id === 4 ? '2026-07-30T00:00:00Z' : undefined,
    }))
    const wrapper = mount(ModelsView)
    await flushPromises()

    const buttons = wrapper.findAll('[data-testid="model-enable"]')
    expect((buttons[0]?.element as HTMLButtonElement).disabled).toBe(true)
    expect((buttons[1]?.element as HTMLButtonElement).disabled).toBe(true)
    expect((buttons[2]?.element as HTMLButtonElement).disabled).toBe(false)
    expect(wrapper.text()).toContain('需要先完成真实音频能力测试')

    await buttons[2]?.trigger('click')
    await flushPromises()
    expect(modelsApi.patch).toHaveBeenCalledWith(4, { enabled: true })
  })

  it('allows disabling an enabled audio model even if its verification timestamp is absent', async () => {
    vi.mocked(modelsApi.list).mockResolvedValue({
      data: [makeModel({ id: 5, public_id: 'legacy-asr', kind: 'asr', enabled: true })],
    })
    vi.mocked(modelsApi.patch).mockResolvedValue(
      makeModel({ id: 5, public_id: 'legacy-asr', kind: 'asr', enabled: false }),
    )
    const wrapper = mount(ModelsView)
    await flushPromises()

    const disable = wrapper.get('[data-testid="model-enable"]')
    expect((disable.element as HTMLButtonElement).disabled).toBe(false)
    await disable.trigger('click')
    await flushPromises()

    expect(modelsApi.patch).toHaveBeenCalledWith(5, { enabled: false })
  })

  it('allows enabling and disabling an OpenCodeFree model', async () => {
    const model = makeModel({
      id: 7,
      public_id: 'opencodefree/model-free',
      provider: 'opencodefree',
      enabled: false,
    })
    vi.mocked(modelsApi.list).mockResolvedValue({ data: [model] })
    vi.mocked(modelsApi.patch)
      .mockResolvedValueOnce({ ...model, enabled: true })
      .mockResolvedValueOnce({ ...model, enabled: false })
    const wrapper = mount(ModelsView)
    await flushPromises()

    const toggle = wrapper.get('[data-testid="model-enable"]')
    expect((toggle.element as HTMLButtonElement).disabled).toBe(false)
    await toggle.trigger('click')
    await flushPromises()
    expect(modelsApi.patch).toHaveBeenCalledWith(7, { enabled: true })
    expect(wrapper.get('[data-testid="model-enable"]').text()).toContain('停用')

    await wrapper.get('[data-testid="model-enable"]').trigger('click')
    await flushPromises()
    expect(modelsApi.patch).toHaveBeenLastCalledWith(7, { enabled: false })
    expect(wrapper.get('[data-testid="model-enable"]').text()).toContain('启用')
  })

  it('tests models from every provider in one batch', async () => {
    vi.mocked(modelsApi.list).mockResolvedValue({
      data: [
        makeModel({ id: 1, public_id: 'nvidia/chat', enabled: true }),
        makeModel({
          id: 2,
          public_id: 'opencodefree/chat',
          provider: 'opencodefree',
          enabled: true,
        }),
      ],
    })
    vi.mocked(modelsApi.createTestJob).mockResolvedValue({
      id: 'job-1',
      mode: 'concurrent',
      status: 'completed',
      total: 2,
      completed: 2,
      results: [
        { model_id: 1, public_id: 'nvidia/chat', provider: 'nvidia', status: 'success' },
        { model_id: 2, public_id: 'opencodefree/chat', provider: 'opencodefree', status: 'failed', error: '上游多次未返回可用响应' },
      ],
    })
    const wrapper = mount(ModelsView)
    await flushPromises()

    await wrapper.get('[data-testid="select-enabled-test-models"]').trigger('click')
    await flushPromises()

    // Both providers stay selected: the backend routes each model on its own.
    expect((wrapper.get('[data-testid="test-model-1"]').element as HTMLInputElement).checked).toBe(true)
    expect((wrapper.get('[data-testid="test-model-2"]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.get('[data-testid="start-model-test"]').text()).toContain('测试 2 个模型')

    await wrapper.get('[data-testid="start-model-test"]').trigger('click')
    await flushPromises()

    expect(modelsApi.createTestJob).toHaveBeenCalledWith({
      model_ids: [1, 2],
      mode: 'concurrent',
      concurrency: 4,
    })
    const job = wrapper.get('[data-testid="model-test-job"]')
    expect(job.text()).toContain('NVIDIA')
    expect(job.text()).toContain('OpenCodeFree')
  })

  it.each([
    ['a null response', null],
    ['an invalid model response', { ...makeModel(), id: null }],
  ])('shows an error and keeps the model unchanged for %s from patch', async (_name, response) => {
    vi.mocked(modelsApi.list).mockResolvedValue({ data: [makeModel({ id: 5 })] })
    vi.mocked(modelsApi.patch).mockResolvedValue(response as never)
    const wrapper = mount(ModelsView)
    await flushPromises()

    await wrapper.get('[data-testid="model-enable"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toContain('更新模型状态失败')
    expect(wrapper.get('[data-testid="model-cards"]').text()).toContain('Chat')
    expect(wrapper.get('[data-testid="model-cards"]').text()).toContain('启用')
  })

  it('shows every key-model block and refreshes relations after manual recovery', async () => {
    vi.mocked(modelsApi.list)
      .mockResolvedValueOnce({ data: [makeModel({ id: 3, blocked_by_key_ids: [9, 10] })] })
      .mockResolvedValueOnce({ data: [makeModel({ id: 3, blocked_by_key_ids: [10] })] })
    vi.mocked(modelsApi.unblock).mockResolvedValue(makeModel({ id: 3 }))
    const wrapper = mount(ModelsView)
    await flushPromises()

    expect(wrapper.get('[data-testid="model-cards"]').text()).toContain('Key #9')
    expect(wrapper.get('[data-testid="model-cards"]').text()).toContain('Key #10')
    await wrapper.get('[data-testid="model-unblock-9"]').trigger('click')
    await flushPromises()

    expect(modelsApi.unblock).toHaveBeenCalledWith(9, 3)
    expect(modelsApi.list).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-testid="model-cards"]').text()).not.toContain('Key #9')
    expect(wrapper.get('[data-testid="model-cards"]').text()).toContain('Key #10')
  })
  it('imports only newly selected candidates so existing model state is not overwritten', async () => {
    const existing = makeModel({
      id: 6,
      public_id: 'speech-public',
      upstream_id: 'vendor/speech',
      kind: 'tts',
      enabled: true,
      capability_verified_at: '2026-07-30T00:00:00Z',
    })
    vi.mocked(modelsApi.list).mockResolvedValue({ data: [existing] })
    vi.mocked(modelsApi.candidates).mockResolvedValue({
      data: [
        {
          upstream_id: 'vendor/speech',
          display_name: 'Speech',
          kind: 'tts',
          supports_vision: false,
          supports_tools: false,
          supports_reasoning: false,
        },
        {
          upstream_id: 'vendor/new-chat',
          display_name: 'New Chat',
          kind: 'chat',
          supports_vision: true,
          supports_tools: true,
          supports_reasoning: false,
        },
      ],
    })
    vi.mocked(modelsApi.save).mockResolvedValue({ saved: 1 })
    const wrapper = mount(ModelsView)
    await flushPromises()

    await wrapper.get('[data-testid="discover-models"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="candidate-vendor/new-chat"]').setValue(true)
    await wrapper.get('[data-testid="save-candidates"]').trigger('click')
    await flushPromises()

    expect(modelsApi.save).toHaveBeenCalledWith([
      {
        upstream_id: 'vendor/new-chat',
        display_name: 'New Chat',
        kind: 'chat',
        supports_vision: true,
        supports_tools: true,
        supports_reasoning: false,
        public_id: 'vendor/new-chat',
        enabled: false,
      },
    ])
  })

  it('strips display-only candidate metadata before saving the whitelist', async () => {
    vi.mocked(modelsApi.candidates).mockResolvedValue({
      data: [{
        public_id: 'opencodefree/model-free',
        upstream_id: 'model-free',
        display_name: 'Model Free',
        kind: 'chat',
        provider: 'opencodefree',
        channel: 'opencodefree',
        badge: 'OpenCodeFree',
        status: 'pending',
        capabilities: ['chat', 'free'],
        supports_vision: false,
        supports_tools: false,
        supports_reasoning: false,
      } as never],
    })
    vi.mocked(modelsApi.save).mockResolvedValue({ saved: 1 })
    const wrapper = mount(ModelsView)
    await flushPromises()

    await wrapper.get('[data-testid="discover-models"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="candidate-table-opencodefree/model-free"]').setValue(true)
    await wrapper.get('[data-testid="save-candidates"]').trigger('click')
    await flushPromises()

    expect(modelsApi.save).toHaveBeenCalledWith([{
      public_id: 'opencodefree/model-free',
      upstream_id: 'model-free',
      display_name: 'Model Free',
      kind: 'chat',
      provider: 'opencodefree',
      enabled: false,
      supports_vision: false,
      supports_tools: false,
      supports_reasoning: false,
      reasoning_wire_format: undefined,
    }])
  })

  it('saves the context window declaration from the table editor and clears it when left empty', async () => {
    vi.mocked(modelsApi.list).mockResolvedValue({ data: [makeModel({ id: 3, public_id: 'chat-model', display_name: 'Chat' })] })
    vi.mocked(modelsApi.patch).mockResolvedValue({
      ...makeModel({ id: 3, public_id: 'chat-model', display_name: 'Chat' }),
      context_length: 131072,
      blocked_by_key_ids: [],
    })
    const wrapper = mount(ModelsView)
    await flushPromises()

    expect(wrapper.text()).toContain('未声明')

    await wrapper.get('[data-testid="model-edit-context"]').trigger('click')
    await wrapper.get('[data-testid="model-context-input-3"]').setValue('131072')
    await wrapper.get('[data-testid="model-save-context-3"]').trigger('click')
    await flushPromises()

    expect(modelsApi.patch).toHaveBeenCalledWith(3, { context_length: 131072 })
    expect(wrapper.text()).toContain('131072')

    // An empty input declares "unknown" (0) rather than failing the edit.
    vi.mocked(modelsApi.patch).mockClear()
    vi.mocked(modelsApi.patch).mockResolvedValue({
      ...makeModel({ id: 3, public_id: 'chat-model', display_name: 'Chat' }),
      context_length: 0,
      blocked_by_key_ids: [],
    })
    await wrapper.get('[data-testid="model-edit-context"]').trigger('click')
    await wrapper.get('[data-testid="model-context-input-3"]').setValue('')
    await wrapper.get('[data-testid="model-save-context-3"]').trigger('click')
    await flushPromises()

    expect(modelsApi.patch).toHaveBeenCalledWith(3, { context_length: 0 })
    expect(wrapper.text()).toContain('未声明')
  })

  it('requires confirmation to delete a model and removes it from the list', async () => {
    vi.mocked(modelsApi.list).mockResolvedValue({ data: [makeModel({ id: 8, display_name: 'To Delete' })] })
    vi.mocked(modelsApi.delete).mockResolvedValue(undefined)
    const wrapper = mount(ModelsView)
    await flushPromises()

    const deleteBtn = wrapper.get('[data-testid="model-delete-8"]')
    // First click opens the confirm dialog; nothing is deleted yet.
    await deleteBtn.trigger('click')
    expect(modelsApi.delete).not.toHaveBeenCalled()

    // Confirming in the dialog performs the deletion.
    bodyElement<HTMLButtonElement>('[data-testid="confirm-delete-model"]').click()
    await flushPromises()

    expect(modelsApi.delete).toHaveBeenCalledWith(8)
    expect(wrapper.text()).not.toContain('To Delete')
  })

  it('tests a single candidate from its row and shows the verdict', async () => {
    vi.mocked(modelsApi.candidates).mockResolvedValue({
      data: [{
        upstream_id: 'mimo-v2.5-free',
        display_name: 'Mimo',
        kind: 'chat',
        provider: 'opencodefree',
        supports_vision: false,
        supports_tools: false,
        supports_reasoning: false,
      }],
    })
    vi.mocked(modelsApi.testCandidate).mockResolvedValue({ status: 'success', duration_ms: 1234 })
    const wrapper = mount(ModelsView)
    await flushPromises()
    await wrapper.get('[data-testid="discover-models"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-testid="candidate-test-button-opencodefree/mimo-v2.5-free"]').trigger('click')
    await flushPromises()

    expect(modelsApi.testCandidate).toHaveBeenCalledWith({ provider: 'opencodefree', upstream_id: 'mimo-v2.5-free' })
    expect(wrapper.text()).toContain('可用 · 1.2 s')
  })

  it('shows the failure reason when a candidate probe fails', async () => {
    vi.mocked(modelsApi.candidates).mockResolvedValue({
      data: [{
        upstream_id: 'vendor/broken',
        display_name: 'Broken',
        kind: 'chat',
        supports_vision: false,
        supports_tools: false,
        supports_reasoning: false,
      }],
    })
    vi.mocked(modelsApi.testCandidate).mockResolvedValue({ status: 'failed', duration_ms: 90, error: '没有可用的 NVIDIA Key' })
    const wrapper = mount(ModelsView)
    await flushPromises()
    await wrapper.get('[data-testid="discover-models"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-testid="candidate-test-button-vendor/broken"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('不可用 · 没有可用的 NVIDIA Key')
  })

  it('sweeps every selected candidate in a batch and reports the tally', async () => {
    vi.mocked(modelsApi.candidates).mockResolvedValue({
      data: [
        { upstream_id: 'vendor/a', display_name: 'A', kind: 'chat', supports_vision: false, supports_tools: false, supports_reasoning: false },
        { upstream_id: 'vendor/b', display_name: 'B', kind: 'chat', supports_vision: false, supports_tools: false, supports_reasoning: false },
        { upstream_id: 'vendor/c', display_name: 'C', kind: 'chat', supports_vision: false, supports_tools: false, supports_reasoning: false },
      ],
    })
    vi.mocked(modelsApi.testCandidate).mockImplementation(async (request) => {
      if (request.upstream_id === 'vendor/b') {
        return { status: 'failed', duration_ms: 10, error: '模型测试失败' }
      }
      return { status: 'success', duration_ms: 20 }
    })
    const wrapper = mount(ModelsView)
    await flushPromises()
    await wrapper.get('[data-testid="discover-models"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="select-all-candidates"]').trigger('click')
    await wrapper.get('[data-testid="test-selected-candidates"]').trigger('click')
    await flushPromises()

    expect(modelsApi.testCandidate).toHaveBeenCalledTimes(3)
    expect(wrapper.text()).toContain('候选测试完成：2/3 可用。')
  })
})
