import { Call } from '@wailsio/runtime'

export type UpstreamInfoConfig = { type: '' | 'sub2api'; baseUrl?: string }
export type ProviderInfoRef = { kind: string; id: string }
export type ProviderInfoDraft = { apiUrl: string; apiKey: string; upstreamInfo?: UpstreamInfoConfig }
export type InfoState = { status: string; updatedAt?: string; retryAt?: string; stale: boolean }
export type Quota = { limit?: number; used?: number; remaining?: number; unit?: string; window?: string; window_start?: string; reset_at?: string }
export type UsageStats = {
  requests?: number; input_tokens?: number; output_tokens?: number; cache_creation_tokens?: number
  cache_write_tokens?: number; cache_read_tokens?: number; total_tokens?: number
  cost?: number; actual_cost?: number; date?: string; model?: string
}
export type Subscription = {
  daily_usage_usd?: number; weekly_usage_usd?: number; monthly_usage_usd?: number
  daily_limit_usd?: number; weekly_limit_usd?: number; monthly_limit_usd?: number
  weekly_window_start?: string; expires_at?: string
}
export type UpstreamUsage = {
  mode?: string; isValid?: boolean; status?: string; planName?: string; unit?: string
  balance?: number; remaining?: number; quota?: Quota; rate_limits?: Quota[]
  subscription?: Subscription; expires_at?: string
  usage?: { today?: UsageStats; total?: UsageStats; rpm?: number; tpm?: number; average_duration_ms?: number }
  daily_usage?: UsageStats[]; model_stats?: UsageStats[]
}
export type UpstreamBilling = {
  billing_scope: string; group_rate_multiplier?: number; user_rate_multiplier?: number
  resolved_rate_multiplier?: number; effective_rate_multiplier?: number; peak_rate_enabled: boolean
  peak_start?: string; peak_end?: string; peak_rate_multiplier?: number; applied_peak_multiplier?: number
  timezone?: string; observed_at?: string
}
export type ProviderInfo = {
  usage?: UpstreamUsage; billing?: UpstreamBilling; usageState: InfoState; billingState: InfoState
  dailyTimezone: string; modelPeriod: string
}
export const infoTimezone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
export const getProviderInfo = (ref: ProviderInfoRef, force = false): Promise<ProviderInfo> =>
  Call.ByName('codeswitch/services.ProviderInfoService.GetInfo', ref, force, infoTimezone())
export const testProviderInfo = (draft: ProviderInfoDraft): Promise<ProviderInfo> =>
  Call.ByName('codeswitch/services.ProviderInfoService.TestConnection', draft, infoTimezone())
export const infoAmount = (amount?: number, unit?: string) => {
  if (amount == null || !Number.isFinite(amount)) return '—'
  const value = amount.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 4 })
  return unit === 'USD' ? `$${value}` : `${value}${unit ? ` ${unit}` : ''}`
}
export const infoNumber = (value?: number) => value == null ? '—' : value.toLocaleString()
export const infoTime = (value?: string) => {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString()
}
export const infoRate = (value?: number) => value == null ? '—' : `${value.toLocaleString(undefined, { maximumFractionDigits: 4 })}×`
export const infoScope = (usage?: UpstreamUsage) => {
  if (usage?.mode === 'quota_limited') return 'keyQuota'
  if (usage?.balance != null) return 'wallet'
  if (usage?.subscription || usage?.planName) return 'subscription'
  return 'remaining'
}
export const infoRemaining = (usage?: UpstreamUsage) => usage?.balance ?? usage?.quota?.remaining ?? usage?.remaining

// Only the non-wallet remaining field uses -1 as an unlimited sentinel.
export const infoUnlimited = (usage?: UpstreamUsage) => usage?.balance == null && infoRemaining(usage) === -1
