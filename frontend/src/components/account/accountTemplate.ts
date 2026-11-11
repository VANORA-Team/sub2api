import type { Account } from '@/types'

export function supportsAccountKeyTemplate(account: Account | null): boolean {
  if (!account || account.parent_account_id != null) return false
  if (account.type === 'apikey' || account.type === 'upstream') return true
  return (account.type === 'bedrock' || account.type === 'service_account') &&
    account.credentials?.auth_mode === 'apikey'
}

export function parseAccountTemplateKeys(text: string): string[] {
  return text.split(/\r?\n/).map(line => line.trim()).filter(Boolean)
}

export function accountTemplateName(prefix: string, index: number): string {
  const suffix = ` ${index + 1}`
  return Array.from(prefix.trim()).slice(0, 100 - suffix.length).join('') + suffix
}
