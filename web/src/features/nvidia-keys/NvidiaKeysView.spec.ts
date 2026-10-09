import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { clearToasts, toastState } from '../../shared/toast'
import { nvidiaKeysApi } from './api'
import BatchImportDialog from './BatchImportDialog.vue'
import NvidiaKeysView from './NvidiaKeysView.vue'
import type { KeyTestResult, NVIDIAKey, NVIDIAKeysResponse, SingleImportResponse } from './types'

vi.mock('vue-router', () => ({
  useRoute: vi.fn(() => ({ query: {} })),
  useRouter: vi.fn(() => ({ replace: vi.fn() })),
}))

vi.mock('./api', () => ({
  nvidiaKeysApi: {
    list: vi.fn(),
    importOne: vi.fn(),
    importBatch: vi.fn(),
    test: vi.fn(),
    testAll: vi.fn(),
    setEnabled: vi.fn(),
    remove: vi.fn(),
  },
}))

function makeKey(overrides: Partial<NVIDIAKey> = {}): NVIDIAKey {
  return {
    id: 7,
    masked: 'nvapi…1234',
    enabled: true,
    auth_invalid: false,
    cooldown_level: 0,
    consecutive_failures: 0,
    created_at: '2026-07-30T00:00:00Z',
    updated_at: '2026-07-30T00:00:00Z',
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
  clearToasts()
  vi.mocked(nvidiaKeysApi.list).mockResolvedValue({ data: [] })
})

afterEach(() => {
  document.body.innerHTML = ''
})

function hasErrorToast(needle: string): boolean {
  return toastState.toasts.some((toast) => toast.type === 'error' && toast.message.includes(needle))
}

function bodyElement<T extends Element>(selector: string): T {
  const element = document.body.querySelector<T>(selector)
  if (!element) throw new Error(`Expected body element: ${selector}`)
  return element
}

describe('NvidiaKeysView', () => {
  it.each([
    ['a non-array data field', { data: null }],
    ['a non-numeric key id', { data: [makeKey({ id: null as never })] }],
  ])('shows a visible error toast for %s in a successful response', async (_name, response) => {
    vi.mocked(nvidiaKeysApi.list).mockResolvedValue(response as never)
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    expect(hasErrorToast('NVIDIA Key 列表加载失败')).toBe(true)
    expect(wrapper.text()).not.toContain('nvapi…1234')
  })

  it('keeps the newest list when an older request resolves last', async () => {
    const first = deferred<NVIDIAKeysResponse>()
    const second = deferred<NVIDIAKeysResponse>()
    vi.mocked(nvidiaKeysApi.list)
      .mockReset()
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise)
    const wrapper = mount(NvidiaKeysView)

    wrapper.getComponent(BatchImportDialog).vm.$emit('imported')
    expect(nvidiaKeysApi.list).toHaveBeenCalledTimes(2)

    second.resolve({ data: [makeKey({ id: 8, masked: 'nvapi…new' })] })
    await flushPromises()
    first.resolve({ data: [makeKey({ masked: 'nvapi…old' })] })
    await flushPromises()

    expect(wrapper.text()).toContain('nvapi…new')
    expect(wrapper.text()).not.toContain('nvapi…old')
  })

  it('does not update list state after unmount', async () => {
    const request = deferred<NVIDIAKeysResponse>()
    vi.mocked(nvidiaKeysApi.list).mockReset().mockReturnValueOnce(request.promise)
    const wrapper = mount(NvidiaKeysView)
    const state = wrapper.vm as unknown as { keys: NVIDIAKey[]; loading: boolean }

    wrapper.unmount()
    request.resolve({ data: [makeKey()] })
    await flushPromises()

    expect(state.keys).toEqual(null)
    expect(state.loading).toBe(true)
  })

  it('does not start a post-import reload after unmount', async () => {
    const request = deferred<SingleImportResponse>()
    vi.mocked(nvidiaKeysApi.importOne).mockReturnValueOnce(request.promise)
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    await wrapper.get('[name="nvidia-key"]').setValue('nvapi-fixture-not-a-real-key')
    await wrapper.get('[data-testid="single-import-form"]').trigger('submit')
    wrapper.unmount()
    request.resolve({ masked: 'nvapi…late', status: 'imported' })
    await flushPromises()

    expect(nvidiaKeysApi.list).toHaveBeenCalledOnce()
  })

  it.each([
    ['a null response', null],
    ['an invalid result item', { id: null, status: 'ok' }],
  ])('shows an error toast and keeps the dialog closed for %s from a single-key test', async (_name, response) => {
    vi.mocked(nvidiaKeysApi.list).mockResolvedValue({ data: [makeKey()] })
    vi.mocked(nvidiaKeysApi.test).mockResolvedValue(response as never)
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="key-card-test"]').trigger('click')
    await flushPromises()

    expect(hasErrorToast('NVIDIA Key 测试失败')).toBe(true)
    expect(document.body.querySelector('[data-testid="key-test-results"]')).toBeNull()
    expect((wrapper.vm as unknown as { testResults: KeyTestResult[] }).testResults).toEqual([])
  })

  it('keeps the test-all loading state separate from list loading', async () => {
    const request = deferred<{ data: KeyTestResult[] }>()
    vi.mocked(nvidiaKeysApi.list).mockResolvedValue({ data: [makeKey()] })
    vi.mocked(nvidiaKeysApi.testAll).mockReturnValue(request.promise)
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    const button = wrapper.get('[data-testid="test-all-keys"]')
    await button.trigger('click')
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.text()).toContain('测活中')
    expect((wrapper.vm as unknown as { testingAll: boolean }).testingAll).toBe(true)

    request.resolve({ data: [] })
    await flushPromises()
    expect((wrapper.vm as unknown as { testingAll: boolean }).testingAll).toBe(false)
  })

  it.each([
    ['a null data field', { data: null }],
    ['an invalid result item', { data: [{ id: null, status: 'ok' }] }],
  ])('shows an error toast and keeps the dialog closed for %s from test-all', async (_name, response) => {
    vi.mocked(nvidiaKeysApi.list).mockResolvedValue({ data: [makeKey()] })
    vi.mocked(nvidiaKeysApi.testAll).mockResolvedValue(response as never)
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="test-all-keys"]').trigger('click')
    await flushPromises()

    expect(hasErrorToast('批量测活失败')).toBe(true)
    expect(document.body.querySelector('[data-testid="key-test-results"]')).toBeNull()
    expect((wrapper.vm as unknown as { testResults: KeyTestResult[] }).testResults).toEqual([])
  })

  it('shows an error and does not render an invalid single import result', async () => {
    vi.mocked(nvidiaKeysApi.importOne).mockResolvedValue({ masked: null, status: 'imported' } as never)
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    await wrapper.get('[name="nvidia-key"]').setValue('nvapi-fixture-not-a-real-key')
    await wrapper.get('[data-testid="single-import-form"]').trigger('submit')
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toContain('NVIDIA Key 导入失败')
    expect(wrapper.text()).not.toContain('imported')
  })

  it('renders responsive key views with cooldown and recent error metadata, without secret actions', async () => {
    vi.mocked(nvidiaKeysApi.list).mockResolvedValue({
      data: [makeKey({
        cooldown_until: '2026-07-30T01:00:00Z',
        cooldown_reason: 'rate_limited',
        consecutive_failures: 2,
        last_error_code: 'upstream_rate_limited',
        last_error_at: '2026-07-30T00:30:00Z',
      })],
    })
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    const table = wrapper.get('[data-testid="key-table"]')
    const cards = wrapper.get('[data-testid="key-cards"]')
    expect(table.classes()).toEqual(expect.arrayContaining(['hidden', 'md:block']))
    expect(cards.classes()).toEqual(expect.arrayContaining(['md:hidden']))
    expect(table.text()).toContain('nvapi…1234')
    expect(table.text()).toContain('2026-07-30T01:00:00Z')
    expect(cards.text()).toContain('upstream_rate_limited')
    expect(cards.text()).toContain('2026-07-30T00:30:00Z')
    expect(wrapper.text()).not.toMatch(/复制|查看明文|导出/)
  })

  it('clears the single import secret before the request settles and never restores it to the DOM', async () => {
    let resolveRequest: ((value: SingleImportResponse) => void) | undefined
    vi.mocked(nvidiaKeysApi.importOne).mockReturnValue(
      new Promise((resolve) => {
        resolveRequest = resolve
      }) as ReturnType<typeof nvidiaKeysApi.importOne>,
    )
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    const secret = 'nvapi-fixture-ui-redaction'
    await wrapper.get('[name="nvidia-key"]').setValue(secret)
    await wrapper.get('[data-testid="single-import-form"]').trigger('submit')

    expect((wrapper.get('[name="nvidia-key"]').element as HTMLInputElement).value).toBe('')
    expect(wrapper.html()).not.toContain(secret)
    expect(nvidiaKeysApi.importOne).toHaveBeenCalledWith(secret)

    resolveRequest?.({ line: 1, masked: 'nvapi…main', status: 'imported' })
    await flushPromises()

    expect(wrapper.text()).toContain('nvapi…main')
    expect(wrapper.html()).not.toContain(secret)
  })

  it('requires confirmation before deleting a key', async () => {
    vi.mocked(nvidiaKeysApi.list).mockResolvedValue({ data: [makeKey()] })
    vi.mocked(nvidiaKeysApi.remove).mockResolvedValue(undefined)
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    // First click opens the confirm dialog; removal is not yet issued.
    await wrapper.get('[data-testid="key-card-delete"]').trigger('click')
    expect(nvidiaKeysApi.remove).not.toHaveBeenCalled()

    // Confirming in the dialog performs the removal.
    bodyElement<HTMLButtonElement>('[data-testid="confirm-delete-key"]').click()
    await flushPromises()
    expect(nvidiaKeysApi.remove).toHaveBeenCalledWith(7)
  })

  it('supports mobile enable, manual test and delete actions and shows the desktop batch hint', async () => {
    const key = makeKey({ id: 8, masked: 'nvapi…5678' })
    vi.mocked(nvidiaKeysApi.list).mockResolvedValue({ data: [key] })
    vi.mocked(nvidiaKeysApi.setEnabled).mockResolvedValue({ id: 8, enabled: false })
    vi.mocked(nvidiaKeysApi.test).mockResolvedValue({ id: 8, status: 'ok' })
    vi.mocked(nvidiaKeysApi.remove).mockResolvedValue(undefined)
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    expect(wrapper.get('[data-testid="mobile-batch-hint"]').text()).toContain('桌面端')
    await wrapper.get('[data-testid="key-card-toggle"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="key-card-test"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="key-card-delete"]').trigger('click')
    bodyElement<HTMLButtonElement>('[data-testid="confirm-delete-key"]').click()
    await flushPromises()

    expect(nvidiaKeysApi.setEnabled).toHaveBeenCalledWith(8, false)
    expect(nvidiaKeysApi.test).toHaveBeenCalledWith(8)
    expect(nvidiaKeysApi.remove).toHaveBeenCalledWith(8)
  })

  it('shows every result returned by the sequential test-all action', async () => {
    vi.mocked(nvidiaKeysApi.list).mockResolvedValue({ data: [makeKey(), makeKey({ id: 8 })] })
    vi.mocked(nvidiaKeysApi.testAll).mockResolvedValue({
      data: [
        { id: 7, status: 'ok', models: ['vendor/chat'] },
        { id: 8, status: 'invalid', reason: 'authentication failed' },
      ],
    })
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="test-all-keys"]').trigger('click')
    await flushPromises()

    const dialog = bodyElement<HTMLElement>('[data-testid="key-test-results"]')
    expect(dialog.textContent).toContain('#7')
    expect(dialog.textContent).toContain('#8')
    expect(dialog.textContent).toContain('authentication failed')
  })

  it('surfaces a persistent load error with retry instead of an empty key list', async () => {
    vi.mocked(nvidiaKeysApi.list)
      .mockRejectedValueOnce(new Error('service unavailable'))
      .mockResolvedValueOnce({ data: [makeKey({ id: 9, masked: 'nvapi…9999' })] })
    const wrapper = mount(NvidiaKeysView)
    await flushPromises()

    const panel = wrapper.get('[data-testid="nvidia-keys-load-error"]')
    expect(panel.text()).toContain('加载失败')

    await wrapper.get('[data-testid="nvidia-keys-retry"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="nvidia-keys-load-error"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('nvapi…9999')
  })
})
