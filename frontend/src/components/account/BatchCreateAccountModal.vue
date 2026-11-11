<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.batchFromTemplate.title')"
    width="wide"
    :close-on-escape="!busy"
    :show-close-button="!busy"
    @close="close"
  >
    <div class="space-y-4">
      <p class="text-sm text-gray-700 dark:text-gray-200">
        {{ t('admin.accounts.batchFromTemplate.source', { name: account?.name }) }}
      </p>
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.accounts.batchFromTemplate.hint') }}</p>
      <form v-if="!rows.length" id="batch-create-account-form" class="space-y-4" @submit.prevent="submit">
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.batchFromTemplate.namePrefix') }}</span>
          <input v-model="namePrefix" class="input w-full" data-testid="batch-name-prefix" autocomplete="off" required />
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.batchFromTemplate.keys') }}</span>
          <textarea
            v-model="keysText"
            class="input w-full font-mono"
            rows="7"
            data-testid="batch-api-keys"
            autocomplete="off"
            autocapitalize="off"
            :spellcheck="false"
            :placeholder="t('admin.accounts.batchFromTemplate.keysPlaceholder')"
            required
          ></textarea>
        </label>
        <p class="input-hint">{{ t('admin.accounts.batchFromTemplate.count', { count: keys.length }) }}</p>
        <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-200">
          <input v-model="schedulable" type="checkbox" data-testid="batch-schedulable" class="h-4 w-4 rounded border-gray-300" />
          {{ t('admin.accounts.batchFromTemplate.schedulable') }}
        </label>
      </form>
      <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <template v-if="rows.length">
        <p role="status" class="text-sm text-gray-700 dark:text-gray-200" data-testid="batch-summary">
          {{ t('admin.accounts.batchFromTemplate.summary', { success: successCount, failed: failedCount, total: rows.length }) }}
        </p>
        <div class="max-h-80 overflow-auto rounded-lg border border-gray-200 dark:border-dark-600">
          <table class="w-full text-left text-sm">
            <thead class="bg-gray-50 text-gray-500 dark:bg-dark-800 dark:text-gray-400">
              <tr>
                <th class="px-3 py-2">{{ t('admin.accounts.accountName') }}</th>
                <th class="px-3 py-2">{{ t('common.status') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in rows" :key="row.operationKey" class="border-t border-gray-100 dark:border-dark-700" data-testid="batch-result">
                <td class="break-all px-3 py-2">{{ row.name }}</td>
                <td class="px-3 py-2" :class="row.status === 'failed' ? 'text-red-600 dark:text-red-400' : ''">
                  {{ statusLabel(row.status) }}
                  <span v-if="row.error" class="mt-1 block text-xs">{{ row.error }}</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </div>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <button type="button" class="btn btn-secondary" :disabled="busy" data-testid="batch-close" @click="close">{{ t('common.close') }}</button>
        <button v-if="!rows.length" type="submit" form="batch-create-account-form" class="btn btn-primary" :disabled="busy || !keys.length" data-testid="batch-submit">
          {{ t('admin.accounts.batchFromTemplate.create') }}
        </button>
        <button v-else-if="failedCount" type="button" class="btn btn-primary" :disabled="busy" data-testid="batch-retry" @click="run">
          {{ t('admin.accounts.batchFromTemplate.retry') }}
        </button>
        <span v-if="busy" class="self-center text-sm text-gray-500">{{ t('common.loading') }}</span>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { createFromTemplate } from '@/api/admin/accounts'
import { extractApiErrorCode, extractApiErrorMessage } from '@/utils/apiError'
import { accountTemplateName, parseAccountTemplateKeys, supportsAccountKeyTemplate } from './accountTemplate'
import type { Account } from '@/types'

type ItemStatus = 'pending' | 'creating' | 'success' | 'failed'
interface BatchItem {
  name: string
  apiKey: string
  operationKey: string
  status: ItemStatus
  error: string
}

const props = defineProps<{ show: boolean; account: Account | null }>()
const emit = defineEmits<{ (event: 'close'): void; (event: 'created'): void }>()
const { t } = useI18n()
const namePrefix = ref('')
const keysText = ref('')
const schedulable = ref(false)
const rows = ref<BatchItem[]>([])
const busy = ref(false)
const error = ref('')
const keys = computed(() => parseAccountTemplateKeys(keysText.value))
const successCount = computed(() => rows.value.filter(row => row.status === 'success').length)
const failedCount = computed(() => rows.value.filter(row => row.status === 'failed').length)
let session = 0

function clearSecrets() {
  keysText.value = ''
  for (const row of rows.value) row.apiKey = ''
}

watch(() => [props.show, props.account?.id] as const, () => {
  session++
  clearSecrets()
  rows.value = []
  error.value = ''
  busy.value = false
  namePrefix.value = props.account?.name ?? ''
  schedulable.value = props.account?.schedulable ?? false
}, { immediate: true, flush: 'sync' })
onBeforeUnmount(() => { session++; clearSecrets() })

function close() {
  if (busy.value) return
  session++
  clearSecrets()
  rows.value = []
  emit('close')
}

async function submit() {
  if (busy.value || rows.value.length) return
  error.value = ''
  if (!supportsAccountKeyTemplate(props.account)) {
    error.value = t('admin.accounts.batchFromTemplate.unsupported')
    return
  }
  if (!namePrefix.value.trim() || /\p{Cc}/u.test(namePrefix.value)) {
    error.value = t('admin.accounts.batchFromTemplate.nameRequired')
    return
  }
  const parsed = keys.value
  if (!parsed.length || parsed.length > 100 || parsed.some(key => key.length > 16384 || /[\s\p{Cc}]/u.test(key))) {
    error.value = t('admin.accounts.batchFromTemplate.invalidKeys')
    return
  }
  if (new Set(parsed).size !== parsed.length) {
    error.value = t('admin.accounts.batchFromTemplate.duplicateKeys')
    return
  }
  rows.value = parsed.map((apiKey, index) => ({
    name: accountTemplateName(namePrefix.value, index),
    apiKey,
    operationKey: `account-template-${globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`}`,
    status: 'pending',
    error: ''
  }))
  keysText.value = ''
  await run()
}

async function run() {
  if (busy.value || !props.account) return
  const currentSession = session
  const sourceID = props.account.id
  const enableScheduling = schedulable.value
  busy.value = true
  let created = false
  try {
    for (const row of rows.value) {
      if (session !== currentSession) break
      if (row.status === 'success') continue
      row.status = 'creating'
      row.error = ''
      try {
        await createFromTemplate(sourceID, { name: row.name, api_key: row.apiKey, schedulable: enableScheduling }, row.operationKey)
        created = true
        if (session !== currentSession) break
        row.status = 'success'
        row.apiKey = ''
      } catch (cause) {
        if (session !== currentSession) break
        row.status = 'failed'
        row.error = extractApiErrorCode(cause) === 'ACCOUNT_TEMPLATE_KEY_UNCHANGED'
          ? t('admin.accounts.batchFromTemplate.keyUnchanged')
          : extractApiErrorMessage(cause, t('admin.accounts.batchFromTemplate.failed'))
        // An upstream error must never echo the submitted secret into the results.
        if (row.apiKey) row.error = row.error.split(row.apiKey).join('***')
      }
    }
  } finally {
    if (session === currentSession) busy.value = false
    if (created) emit('created')
  }
}

function statusLabel(status: ItemStatus): string {
  switch (status) {
    case 'pending': return t('admin.accounts.batchFromTemplate.pending')
    case 'creating': return t('admin.accounts.batchFromTemplate.creating')
    case 'success': return t('admin.accounts.batchFromTemplate.success')
    case 'failed': return t('admin.accounts.batchFromTemplate.failed')
  }
}
</script>
