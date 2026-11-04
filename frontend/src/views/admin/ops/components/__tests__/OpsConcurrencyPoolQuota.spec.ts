import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpsConcurrencyCard from '../OpsConcurrencyCard.vue'

const getConcurrencyStats = vi.fn()
const getAccountAvailabilityStats = vi.fn()

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    getConcurrencyStats: (...args: unknown[]) => getConcurrencyStats(...args),
    getAccountAvailabilityStats: (...args: unknown[]) => getAccountAvailabilityStats(...args),
    getUserConcurrencyStats: vi.fn()
  }
}))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => params ? `${key} ${JSON.stringify(params)}` : key
  })
}))

describe('OpsConcurrencyCard pool quota', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getConcurrencyStats.mockResolvedValue({ enabled: true, account: {}, group: {}, platform: {} })
    getAccountAvailabilityStats.mockResolvedValue({
      enabled: true,
      platform: {},
      group: { 7: { pool_quota: { known: 1, low: 1, balance_low: 1, unknown: 1, unsupported: 1 } } },
      account: {
        // The first display group differs from the selected group; the server
        // has already filtered the account by its full group membership.
        1: { account_name: 'Claude', group_id: 2, quota_monitored: true,
          quota: { source: 'upstream', observed_at: '2026-11-04T12:00:00Z', partial: true,
            windows: [{ window: '5h', used_percent: 92 }],
            balances: [{ currency: 'USD', balance: 3 }] } },
        2: { account_name: 'Unknown', group_id: 7, quota_monitored: true },
        3: { account_name: 'Unsupported', group_id: 7, quota_unsupported: true,
          local_quota: [{ window: 'total', used_percent: 95, used: 95, limit: 100 }] }
      }
    })
  })

  it('shows provider windows and currencies, and distinguishes unknown and unsupported accounts', async () => {
    const wrapper = mount(OpsConcurrencyCard, { props: { groupIdFilter: 7, refreshToken: 0 } })
    await flushPromises()

    expect(wrapper.text()).toContain('Claude')
    expect(wrapper.text()).toContain('"percent":"92.0"')
    expect(wrapper.text()).toContain('"currency":"USD"')
    expect(wrapper.text()).toContain('admin.ops.concurrency.poolUnknown')
    expect(wrapper.text()).toContain('admin.ops.concurrency.poolPartial')
    expect(wrapper.text()).toContain('admin.ops.concurrency.poolUnsupported')
    expect(wrapper.text()).toContain('admin.ops.concurrency.poolLocalLimitHint')
    expect(wrapper.text()).toContain('admin.ops.concurrency.poolLowBadge')
    expect(wrapper.text()).toContain('"known":1')
    expect(getAccountAvailabilityStats).toHaveBeenCalledWith('', 7)
  })
})
