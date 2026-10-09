import { config, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { clearToasts, toastState } from '../../shared/toast'
import { accessKeysApi } from './api'
import AccessKeysView from './AccessKeysView.vue'
import CreateAccessKeyDialog from './CreateAccessKeyDialog.vue'
import type { AccessKeysResponse } from './types'

vi.mock('vue-router', () => ({
  useRoute: vi.fn(() => ({ query: {} })),
  useRouter: vi.fn(() => ({ replace: vi.fn() })),
}))

vi.mock('./api', () => ({
  accessKeysApi: {
    list: vi.fn(),
    create: vi.fn(),
    updatePolicy: vi.fn(),
    revoke: vi.fn(),
    delete: vi.fn(),
  },
}))

const listedKey = {
  id: 4,
  name: '家庭电脑',
  key_prefix: 'nvr_abcd',
  created_at: '2026-07-30T08:00:00Z',
  last_used_at: '2026-07-30T09:30:00Z',
  rpm_limit: 60,
  tpm_limit: 60000,
  max_concurrent: 5,
  token_budget: 1000000,
  consumed_tokens: 250000,
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise
  })
  return { promise, resolve }
}

beforeEach(() => {
  config.global.stubs = { Teleport: true }
  vi.clearAllMocks()
  clearToasts()
  vi.mocked(accessKeysApi.list).mockResolvedValue({ data: [listedKey] })
  vi.mocked(accessKeysApi.revoke).mockResolvedValue(undefined)
  vi.mocked(accessKeysApi.delete).mockResolvedValue(undefined)
})

afterEach(() => {
  // Keep the Teleport stub local to this suite; leaking it changes later tests' DOM topology.
  config.global.stubs = {}
})

describe('AccessKeysView', () => {
  it.each([
    ['a non-array data field', { data: null }],
    ['a non-numeric key id', { data: [{ ...listedKey, id: null }] }],
  ])('shows a visible error toast for %s in a successful response', async (_name, response) => {
    vi.mocked(accessKeysApi.list).mockResolvedValue(response as never)
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    expect(toastState.toasts.some((toast) => toast.type === 'error' && toast.message.includes('列表加载失败'))).toBe(true)
    expect(wrapper.text()).not.toContain(listedKey.name)
  })

  it('keeps the newest list when an older request resolves last', async () => {
    const first = deferred<AccessKeysResponse>()
    const second = deferred<AccessKeysResponse>()
    vi.mocked(accessKeysApi.list)
      .mockReset()
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise)
    const wrapper = mount(AccessKeysView)

    wrapper.getComponent(CreateAccessKeyDialog).vm.$emit('created')
    expect(accessKeysApi.list).toHaveBeenCalledTimes(2)

    second.resolve({ data: [{ ...listedKey, id: 6, name: '新数据' }] })
    await flushPromises()
    first.resolve({ data: [{ ...listedKey, id: 5, name: '旧数据' }] })
    await flushPromises()

    expect(wrapper.text()).toContain('新数据')
    expect(wrapper.text()).not.toContain('旧数据')
  })

  it('flags an expired key instead of claiming it is valid', async () => {
    vi.mocked(accessKeysApi.list).mockResolvedValue({
      data: [{ ...listedKey, expires_at: '2020-01-01T00:00:00Z' }],
    })
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    const table = wrapper.get('[data-testid="access-key-table"]')
    expect(table.text()).toContain('已过期')
    expect(table.text()).not.toContain('有效')
  })

  it('flags a key whose token budget is exhausted', async () => {
    vi.mocked(accessKeysApi.list).mockResolvedValue({
      data: [{ ...listedKey, token_budget: 1000, consumed_tokens: 1000 }],
    })
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    expect(wrapper.get('[data-testid="access-key-table"]').text()).toContain('预算已耗尽')
  })

  it('shows expiry and budget on mobile cards', async () => {
    vi.mocked(accessKeysApi.list).mockResolvedValue({
      data: [{ ...listedKey, expires_at: '2027-01-01T00:00:00Z', token_budget: 1000000, consumed_tokens: 250000 }],
    })
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    const card = wrapper.get('[data-testid="access-key-cards"]')
    expect(card.text()).toContain('过期时间')
    expect(card.text()).toContain('Token 预算')
    expect(card.text()).toContain('25%')
  })

  it('surfaces a persistent load error with retry instead of an empty state', async () => {
    vi.mocked(accessKeysApi.list)
      .mockRejectedValueOnce(new Error('network down'))
      .mockResolvedValueOnce({ data: [listedKey] })
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    const panel = wrapper.get('[data-testid="access-keys-load-error"]')
    expect(panel.text()).toContain('列表加载失败')
    expect(wrapper.text()).not.toContain('尚未创建')

    await wrapper.get('[data-testid="access-keys-retry"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="access-keys-load-error"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('家庭电脑')
  })

  it('does not update list state after unmount', async () => {
    const request = deferred<AccessKeysResponse>()
    vi.mocked(accessKeysApi.list).mockReset().mockReturnValueOnce(request.promise)
    const wrapper = mount(AccessKeysView)
    const state = wrapper.vm as unknown as { keys: typeof listedKey[]; loading: boolean }

    wrapper.unmount()
    request.resolve({ data: [listedKey] })
    await flushPromises()

    expect(state.keys).toEqual(null)
    expect(state.loading).toBe(true)
  })

  it('does not start a post-mutation reload after unmount', async () => {
    const revoke = deferred<void>()
    vi.mocked(accessKeysApi.revoke).mockReset().mockReturnValueOnce(revoke.promise)
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="revoke-access-key-4"]').trigger('click')
    await wrapper.get('[data-testid="confirm-access-key-action"]').trigger('click')
    wrapper.unmount()
    revoke.resolve(undefined)
    await flushPromises()

    expect(accessKeysApi.list).toHaveBeenCalledOnce()
  })

  it('shows a newly created plaintext once, copies it, and cannot recover it after closing', async () => {
    const plaintext = 'nvr_once_only_secret'
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })
    vi.mocked(accessKeysApi.create).mockResolvedValue({
      ...listedKey,
      id: 5,
      name: 'CI',
      key_prefix: 'nvr_ci12',
      key: plaintext,
    })
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="open-create-access-key"]').trigger('click')
    await wrapper.get('[data-testid="access-key-name"]').setValue('CI')
    await wrapper.get('[data-testid="create-access-key-form"]').trigger('submit')
    await flushPromises()

    expect(wrapper.get('[data-testid="created-access-key"]').text()).toContain(plaintext)
    await wrapper.get('[data-testid="copy-created-access-key"]').trigger('click')
    expect(writeText).toHaveBeenCalledWith(plaintext)

    await wrapper.get('[data-testid="close-created-access-key"]').trigger('click')
    expect(wrapper.text()).not.toContain(plaintext)
    expect(accessKeysApi.list).toHaveBeenCalledTimes(2)
    expect(JSON.stringify(vi.mocked(accessKeysApi.list).mock.results)).not.toContain(plaintext)

    await wrapper.get('[data-testid="open-create-access-key"]').trigger('click')
    expect(wrapper.findAll('[data-testid="created-access-key"]')).toHaveLength(0)
    expect(wrapper.findAll('[data-testid="create-access-key-form"]')).toHaveLength(1)
  })

  it('clears the temporary textarea after a successful legacy copy and keeps plaintext visible', async () => {
    const plaintext = 'nvr_legacy_success_secret'
    let textarea: HTMLTextAreaElement | undefined
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: undefined,
    })
    Object.defineProperty(document, 'execCommand', {
      configurable: true,
      value: vi.fn().mockImplementation(() => {
        const activeTextarea = document.querySelector('textarea') as HTMLTextAreaElement
        textarea = activeTextarea
        expect(activeTextarea.value).toBe(plaintext)
        expect(document.body.contains(activeTextarea)).toBe(true)
        return true
      }),
    })
    vi.mocked(accessKeysApi.create).mockResolvedValue({
      ...listedKey,
      id: 5,
      name: 'CI',
      key_prefix: 'nvr_ci12',
      key: plaintext,
    })
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="open-create-access-key"]').trigger('click')
    await wrapper.get('[data-testid="access-key-name"]').setValue('CI')
    await wrapper.get('[data-testid="create-access-key-form"]').trigger('submit')
    await flushPromises()
    await wrapper.get('[data-testid="copy-created-access-key"]').trigger('click')

    expect(textarea?.value).toBe('')
    expect(document.querySelector('textarea')).toBeNull()
    expect(wrapper.get('[data-testid="created-access-key"]').text()).toContain(plaintext)
    expect(wrapper.text()).toContain('已复制。')
  })

  it('clears the temporary textarea after a failed legacy copy and keeps plaintext visible', async () => {
    const plaintext = 'nvr_legacy_false_secret'
    let textarea: HTMLTextAreaElement | undefined
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: undefined,
    })
    Object.defineProperty(document, 'execCommand', {
      configurable: true,
      value: vi.fn().mockImplementation(() => {
        textarea = document.querySelector('textarea') as HTMLTextAreaElement
        return false
      }),
    })
    vi.mocked(accessKeysApi.create).mockResolvedValue({
      ...listedKey,
      id: 5,
      name: 'CI',
      key_prefix: 'nvr_ci12',
      key: plaintext,
    })
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="open-create-access-key"]').trigger('click')
    await wrapper.get('[data-testid="access-key-name"]').setValue('CI')
    await wrapper.get('[data-testid="create-access-key-form"]').trigger('submit')
    await flushPromises()
    await wrapper.get('[data-testid="copy-created-access-key"]').trigger('click')

    expect(textarea?.value).toBe('')
    expect(document.querySelector('textarea')).toBeNull()
    expect(wrapper.get('[data-testid="created-access-key"]').text()).toContain(plaintext)
    expect(wrapper.text()).toContain('复制失败，请手动复制。')
  })

  it('clears the temporary textarea after a throwing legacy copy and keeps plaintext visible', async () => {
    const plaintext = 'nvr_legacy_throw_secret'
    let textarea: HTMLTextAreaElement | undefined
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: undefined,
    })
    Object.defineProperty(document, 'execCommand', {
      configurable: true,
      value: vi.fn().mockImplementation(() => {
        textarea = document.querySelector('textarea') as HTMLTextAreaElement
        throw new Error('copy failed')
      }),
    })
    vi.mocked(accessKeysApi.create).mockResolvedValue({
      ...listedKey,
      id: 5,
      name: 'CI',
      key_prefix: 'nvr_ci12',
      key: plaintext,
    })
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="open-create-access-key"]').trigger('click')
    await wrapper.get('[data-testid="access-key-name"]').setValue('CI')
    await wrapper.get('[data-testid="create-access-key-form"]').trigger('submit')
    await flushPromises()
    await wrapper.get('[data-testid="copy-created-access-key"]').trigger('click')

    expect(textarea?.value).toBe('')
    expect(document.querySelector('textarea')).toBeNull()
    expect(wrapper.get('[data-testid="created-access-key"]').text()).toContain(plaintext)
    expect(wrapper.text()).toContain('复制失败，请手动复制。')
  })

  it('shows safe metadata and requires two-step confirmation before revoking', async () => {
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    expect(wrapper.text()).toContain('家庭电脑')
    expect(wrapper.text()).toContain('nvr_abcd')
    expect(wrapper.text()).toContain('2026/07/30')
    expect(wrapper.text()).toContain('17:30')

    const revoke = wrapper.get('[data-testid="revoke-access-key-4"]')
    // First click opens the confirm dialog; nothing is revoked yet.
    await revoke.trigger('click')
    expect(accessKeysApi.revoke).not.toHaveBeenCalled()

    // Confirming in the dialog performs the revoke.
    await wrapper.get('[data-testid="confirm-access-key-action"]').trigger('click')
    await flushPromises()
    expect(accessKeysApi.revoke).toHaveBeenCalledWith(4)
  })

  it('pre-fills the policy dialog, saves the updated policy, and reloads the list', async () => {
    vi.mocked(accessKeysApi.updatePolicy).mockResolvedValue(undefined)
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="edit-access-key-policy-4"]').trigger('click')
    await flushPromises()

    const rpm = wrapper.get('[data-testid="access-key-rpm-limit"]')
    expect((rpm.element as HTMLInputElement).value).toBe('60')
    expect((wrapper.get('[data-testid="access-key-tpm-limit"]').element as HTMLInputElement).value).toBe('60000')
    expect((wrapper.get('[data-testid="access-key-max-concurrent"]').element as HTMLInputElement).value).toBe('5')
    expect((wrapper.get('[data-testid="access-key-token-budget"]').element as HTMLInputElement).value).toBe('1000000')
    expect(wrapper.get('[data-testid="access-key-budget-4"]').text()).toContain('K')

    await rpm.setValue('120')
    await wrapper.get('[data-testid="access-key-tpm-limit"]').setValue('120000')
    await wrapper.get('[data-testid="access-key-max-concurrent"]').setValue('10')
    await wrapper.get('[data-testid="access-key-token-budget"]').setValue('2000000')
    await wrapper.get('[data-testid="edit-access-key-policy-form"]').trigger('submit')
    await flushPromises()

    expect(accessKeysApi.updatePolicy).toHaveBeenCalledWith(4, {
      expires_at: null,
      rpm_limit: 120,
      tpm_limit: 120000,
      max_concurrent: 10,
      token_budget: 2000000,
    })
    expect(accessKeysApi.list).toHaveBeenCalledTimes(2)
  })

  it('rejects an out-of-range policy inline without calling the API', async () => {
    vi.mocked(accessKeysApi.updatePolicy).mockResolvedValue(undefined)
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    await wrapper.get('[data-testid="edit-access-key-policy-4"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-testid="access-key-rpm-limit"]').setValue('200000')
    await wrapper.get('[data-testid="access-key-max-concurrent"]').setValue('-1')
    await wrapper.get('[data-testid="edit-access-key-policy-form"]').trigger('submit')
    await flushPromises()

    expect(wrapper.get('[data-testid="access-key-rpm-error"]').text()).toContain('0-100000')
    expect(wrapper.get('[data-testid="access-key-max-concurrent-error"]').text()).toContain('0-10000')
    expect(accessKeysApi.updatePolicy).not.toHaveBeenCalled()
  })

  it('requires confirmation to delete an access key and removes it from the list', async () => {
    const wrapper = mount(AccessKeysView)
    await flushPromises()

    const deleteBtn = wrapper.get('[data-testid="delete-access-key-4"]')
    // First click opens the confirm dialog; nothing is deleted yet.
    await deleteBtn.trigger('click')
    expect(accessKeysApi.delete).not.toHaveBeenCalled()

    // Confirming in the dialog performs the deletion.
    await wrapper.get('[data-testid="confirm-access-key-action"]').trigger('click')
    await flushPromises()

    expect(accessKeysApi.delete).toHaveBeenCalledWith(4)
    expect(wrapper.text()).not.toContain('家庭电脑')
  })
})
