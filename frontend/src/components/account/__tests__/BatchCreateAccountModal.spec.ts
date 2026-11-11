import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import BatchCreateAccountModal from '../BatchCreateAccountModal.vue'
import { accountTemplateName, supportsAccountKeyTemplate } from '../accountTemplate'
import type { Account } from '@/types'

const { createFromTemplate } = vi.hoisted(() => ({ createFromTemplate: vi.fn() }))
vi.mock('@/api/admin/accounts', () => ({ createFromTemplate }))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => params ? `${key} ${JSON.stringify(params)}` : key })
}))

const account = {
  id: 42, name: 'upstream', platform: 'openai', type: 'apikey',
  schedulable: true, parent_account_id: null
} as Account

const mountModal = () => mount(BatchCreateAccountModal, {
  props: { show: true, account },
  global: {
    stubs: {
      BaseDialog: { template: '<div v-if="show"><slot /><slot name="footer" /></div>', props: ['show'] }
    }
  }
})

async function fillAndSubmit(wrapper: ReturnType<typeof mountModal>, text = ['example-token-a', 'example-token-b'].join('\n')) {
  await wrapper.get('[data-testid="batch-name-prefix"]').setValue('new accounts')
  await wrapper.get('[data-testid="batch-api-keys"]').setValue(text)
  await wrapper.get('form').trigger('submit')
  await flushPromises()
}

describe('batch account creation from a template', () => {
  beforeEach(() => {
    createFromTemplate.mockReset()
    createFromTemplate.mockResolvedValue({ id: 43, name: 'created' })
  })

  it('creates one account per nonempty line using the selected template and new keys', async () => {
    const wrapper = mountModal()
    const firstKey = ['new', 'key', '1'].join('-')
    const secondKey = ['new', 'key', '2'].join('-')
    await fillAndSubmit(wrapper, ` ${firstKey} \r\n\r\n${secondKey}\n`)
    expect(createFromTemplate).toHaveBeenCalledTimes(2)
    expect(createFromTemplate.mock.calls[0]).toEqual([
      42, { name: 'new accounts 1', api_key: firstKey, schedulable: true }, expect.stringMatching(/^account-template-/)
    ])
    expect(createFromTemplate.mock.calls[1][1]).toEqual({ name: 'new accounts 2', api_key: secondKey, schedulable: true })
    expect(createFromTemplate.mock.calls[0][2]).not.toBe(createFromTemplate.mock.calls[1][2])
    expect(wrapper.findAll('[data-testid="batch-result"]')).toHaveLength(2)
    expect(wrapper.get('[data-testid="batch-summary"]').text()).toContain('"success":2')
    expect(wrapper.text()).not.toContain(firstKey)
    expect(wrapper.emitted('created')).toHaveLength(1)
    wrapper.unmount()
  })

  it('supports creating accounts with scheduling disabled', async () => {
    const wrapper = mountModal()
    await wrapper.get('[data-testid="batch-schedulable"]').setValue(false)
    await fillAndSubmit(wrapper, 'new-key')
    expect(createFromTemplate.mock.calls[0][1].schedulable).toBe(false)
    wrapper.unmount()
  })

  it('retries only failed items with the same request and operation key', async () => {
    createFromTemplate.mockResolvedValueOnce({ id: 43 }).mockRejectedValueOnce(new Error('timeout'))
    const wrapper = mountModal()
    await fillAndSubmit(wrapper)
    expect(wrapper.get('[data-testid="batch-summary"]').text()).toContain('"failed":1')
    const failedRequest = createFromTemplate.mock.calls[1]
    await wrapper.get('[data-testid="batch-retry"]').trigger('click')
    await flushPromises()
    expect(createFromTemplate).toHaveBeenCalledTimes(3)
    expect(createFromTemplate.mock.calls[2]).toEqual(failedRequest)
    expect(wrapper.get('[data-testid="batch-summary"]').text()).toContain('"success":2')
    expect(wrapper.find('[data-testid="batch-retry"]').exists()).toBe(false)
    expect(wrapper.emitted('created')).toHaveLength(2)
    wrapper.unmount()
  })

  it.each([
    ['same-key\nsame-key', 'duplicateKeys'],
    ['two keys', 'invalidKeys'],
    ['', 'invalidKeys'],
    [Array.from({ length: 101 }, (_, index) => `key-${index}`).join('\n'), 'invalidKeys']
  ])('rejects invalid input before any requests: %s', async (text, expectedError) => {
    const wrapper = mountModal()
    await fillAndSubmit(wrapper, text)
    expect(createFromTemplate).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain(expectedError)
    wrapper.unmount()
  })

  it('blocks duplicate submissions while a request is in flight', async () => {
    let resolve!: (value: unknown) => void
    createFromTemplate.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const wrapper = mountModal()
    await fillAndSubmit(wrapper)
    expect(createFromTemplate).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="batch-close"]').attributes('disabled')).toBeDefined()
    resolve({ id: 43 })
    await flushPromises()
    expect(createFromTemplate).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('stops scheduling subsequent requests when unmounted', async () => {
    let resolve!: (value: unknown) => void
    createFromTemplate.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const wrapper = mountModal()
    await fillAndSubmit(wrapper)
    wrapper.unmount()
    resolve({ id: 43 })
    await flushPromises()
    expect(createFromTemplate).toHaveBeenCalledTimes(1)
  })

  it('discards old secrets and stops the previous batch when the template changes', async () => {
    let resolve!: (value: unknown) => void
    createFromTemplate.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const wrapper = mountModal()
    await fillAndSubmit(wrapper)
    await wrapper.setProps({ account: { ...account, id: 99, name: 'other source', schedulable: false } })
    resolve({ id: 43 })
    await flushPromises()
    expect(createFromTemplate).toHaveBeenCalledTimes(1)
    expect((wrapper.get('[data-testid="batch-api-keys"]').element as HTMLTextAreaElement).value).toBe('')
    expect((wrapper.get('[data-testid="batch-name-prefix"]').element as HTMLInputElement).value).toBe('other source')
    wrapper.unmount()
  })

  it('redacts keys in errors and clears failed keys when closed', async () => {
    createFromTemplate.mockRejectedValueOnce(new Error('bad key new-key'))
    const wrapper = mountModal()
    await fillAndSubmit(wrapper, 'new-key')
    expect(wrapper.text()).not.toContain('new-key')
    await wrapper.get('[data-testid="batch-close"]').trigger('click')
    expect(wrapper.emitted('close')).toHaveLength(1)
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    expect((wrapper.get('[data-testid="batch-api-keys"]').element as HTMLTextAreaElement).value).toBe('')
    wrapper.unmount()
  })

  it('translates the unchanged key error', async () => {
    createFromTemplate.mockRejectedValueOnce({ code: 'ACCOUNT_TEMPLATE_KEY_UNCHANGED', message: 'raw error' })
    const wrapper = mountModal()
    await fillAndSubmit(wrapper, 'new-key')
    expect(wrapper.text()).toContain('batchFromTemplate.keyUnchanged')
    wrapper.unmount()
  })

  it('keeps generated names within 100 Unicode code points', () => {
    const name = accountTemplateName('名'.repeat(100), 99)
    expect(Array.from(name)).toHaveLength(100)
    expect(name.endsWith(' 100')).toBe(true)
  })

  it.each([
    [{ type: 'apikey' }, true],
    [{ type: 'upstream' }, true],
    [{ type: 'bedrock', credentials: { auth_mode: 'apikey' } }, true],
    [{ type: 'service_account', credentials: { auth_mode: 'apikey' } }, true],
    [{ type: 'bedrock', credentials: { auth_mode: 'sigv4' } }, false],
    [{ type: 'service_account' }, false],
    [{ type: 'oauth' }, false],
    [{ parent_account_id: 1 }, false]
  ])('offers templates only for API key authentication: %o', (overrides, expected) => {
    expect(supportsAccountKeyTemplate({ ...account, ...overrides } as Account)).toBe(expected)
  })
})
