import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useAppStore } from '@/stores/app'
import { checkUpdates, performUpdate, getRollbackVersions, rollback } from '@/api/admin/system'
import VersionBadge from '../VersionBadge.vue'

const auth = vi.hoisted(() => ({ isAdmin: true }))

vi.mock('@/stores', async () => {
  const { useAppStore } = await import('@/stores/app')
  return { useAppStore, useAuthStore: () => auth }
})
vi.mock('@/api/admin/system', () => ({
  checkUpdates: vi.fn(),
  performUpdate: vi.fn(),
  restartService: vi.fn(),
  getRollbackVersions: vi.fn(),
  rollback: vi.fn()
}))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
  createI18n: () => ({ global: { t: (key: string) => key } })
}))

enableAutoUnmount(afterEach)

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  auth.isAdmin = true
  vi.mocked(checkUpdates).mockResolvedValue({
    current_version: '0.1.147',
    latest_version: '99.0.0',
    has_update: true,
    cached: false,
    build_type: 'release',
    update_mode: 'external',
    repository_url: 'https://github.com/VANORA-Team/sub2api',
    release_info: {
      name: 'Old upstream release',
      body: '',
      published_at: '',
      html_url: 'https://github.com/ranxi2001/sub2api/releases'
    }
  })
})

describe('image-managed version badge', () => {
  it('links to the fork and hides in-place update and rollback controls', async () => {
    const wrapper = mount(VersionBadge, { global: { stubs: { Icon: true } } })
    await flushPromises()
    await wrapper.get('button').trigger('click')

    expect(wrapper.text()).toContain('v0.1.147')
    expect(wrapper.text()).toContain('version.containerModeHint')
    expect(wrapper.text()).not.toContain('version.upToDate')
    expect(wrapper.text()).not.toContain('version.updateNow')
    expect(wrapper.text()).not.toContain('version.rollback')
    expect(wrapper.text()).not.toContain('99.0.0')
    expect(wrapper.findAll('button')).toHaveLength(1)
    expect(wrapper.get('a').attributes()).toMatchObject({
      href: 'https://github.com/VANORA-Team/sub2api',
      target: '_blank',
      rel: 'noopener noreferrer'
    })
    expect(performUpdate).not.toHaveBeenCalled()
    expect(getRollbackVersions).not.toHaveBeenCalled()
    expect(rollback).not.toHaveBeenCalled()

    const cached = await useAppStore().fetchVersion()
    expect(cached).toMatchObject({
      update_mode: 'external',
      repository_url: 'https://github.com/VANORA-Team/sub2api'
    })
    expect(checkUpdates).toHaveBeenCalledTimes(1)
  })

  it('keeps standalone release update controls available', async () => {
    vi.mocked(checkUpdates).mockResolvedValue({
      current_version: '0.1.147',
      latest_version: '0.1.148',
      has_update: true,
      cached: false,
      build_type: 'release',
      update_mode: 'inline'
    })
    const wrapper = mount(VersionBadge, { global: { stubs: { Icon: true } } })
    await flushPromises()
    await wrapper.get('button').trigger('click')

    expect(wrapper.text()).toContain('version.updateNow')
    expect(wrapper.text()).not.toContain('version.containerModeHint')
  })

  it('keeps the repository link available when version loading fails', async () => {
    vi.mocked(checkUpdates).mockRejectedValue(new Error('version endpoint unavailable'))
    const errorLog = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      const wrapper = mount(VersionBadge, {
        props: { version: '0.1.147' },
        global: { stubs: { Icon: true } }
      })
      await flushPromises()
      await wrapper.get('button').trigger('click')

      expect(wrapper.get('a').attributes('href')).toBe('https://github.com/VANORA-Team/sub2api')
      expect(wrapper.text()).toContain('v0.1.147')
      expect(wrapper.text()).not.toContain('version.updateNow')
    } finally {
      errorLog.mockRestore()
    }
  })
})
