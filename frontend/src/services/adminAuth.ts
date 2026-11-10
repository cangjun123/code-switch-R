import { reactive, readonly } from 'vue'
import { resetEventSource } from '../wails-runtime'

const ADMIN_AUTH_INVALID_EVENT = 'codeswitch-admin-auth-invalid'

type AdminErrorPayload = {
  error?: {
    code?: string
    message?: string
  }
}

export class AdminApiError extends Error {
  status: number
  code?: string

  constructor(message: string, status: number, code?: string) {
    super(message)
    this.name = 'AdminApiError'
    this.status = status
    this.code = code
  }
}

export type AdminAuthStatus = {
  initialized: boolean
  authenticated: boolean
  username?: string
}

export type CodexRelayKeyListItem = {
  id: string
  name: string
  maskedKey: string
  enabled: boolean
  createdAt: string
  tokenLimit: number
  usdLimit: string
  quotaPeriod: 'once' | 'daily' | 'weekly' | 'monthly'
  allowedProviderIds: number[]
  allowedClaudeProviderIds: number[]
  allowedGeminiProviderIds: string[]
  providerBinding?: RelayKeyProviderBinding
  quota?: CodexRelayQuotaStatus
}

export type CodexRelayKeyCreateResult = {
  id: string
  name: string
  key: string
  enabled: boolean
  createdAt: string
  tokenLimit: number
  usdLimit: string
  quotaPeriod: 'once' | 'daily' | 'weekly' | 'monthly'
  allowedProviderIds: number[]
  allowedClaudeProviderIds: number[]
  allowedGeminiProviderIds: string[]
}

export type RelayProviderKind = 'codex' | 'claude' | 'gemini'

// Gemini providers use string IDs and are identified by providerRef.
export type RelayKeyProviderBinding = {
  kind: RelayProviderKind
  providerId: number
  providerRef?: string
}

export type ProviderKeyItem = {
  id: string
  name: string
  maskedKey: string
  enabled: boolean
  kind: RelayProviderKind
  providerId: number
  providerRef?: string
}

export type ProviderKeyEnsureResult = ProviderKeyItem & {
  key: string
  created: boolean
}

export type CodexRelayProviderOption = {
  id: number
  name: string
  enabled: boolean
}

export type CodexRelayQuotaStatus = {
  tokenLimit: number
  tokenUsed: number
  tokenRemaining: number | null
  usdLimit: string
  usdUsed: string
  usdRemaining: string | null
  period: 'once' | 'daily' | 'weekly' | 'monthly'
  windowStartedAt?: string
  resetAt?: string
  serverTimezone: string
  trackingStarted: boolean
  tokenExhausted: boolean
  usdExhausted: boolean
  blocked: boolean
}

export type CodexRelayModelPrice = {
  model: string
  input: string
  cachedInput: string
  output: string
  reasoningOutput: string
  updatedAt?: string
  source?: 'builtin' | 'custom'
  canRestoreDefault?: boolean
}

export type CodexRelayUnpricedModel = {
  model: string
  firstSeenAt: string
  lastSeenAt: string
  callCount: number
}

type AdminAuthState = {
  ready: boolean
  loading: boolean
  initialized: boolean
  authenticated: boolean
  username: string
}

const adminAuthState = reactive<AdminAuthState>({
  ready: false,
  loading: false,
  initialized: false,
  authenticated: false,
  username: '',
})

let statusPromise: Promise<AdminAuthStatus> | null = null

function applyStatus(status: AdminAuthStatus, ready = true) {
  adminAuthState.initialized = !!status.initialized
  adminAuthState.authenticated = !!status.authenticated
  adminAuthState.username = status.authenticated ? status.username ?? '' : ''
  adminAuthState.ready = ready
}

function markUnauthorized() {
  resetEventSource()
  applyStatus({
    initialized: adminAuthState.initialized,
    authenticated: false,
    username: '',
  })
}

async function parseJSON<T>(response: Response): Promise<T | undefined> {
  const text = await response.text()
  if (!text) {
    return undefined
  }

  return JSON.parse(text) as T
}

async function adminRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers ?? {})
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  if (!headers.has('Accept')) {
    headers.set('Accept', 'application/json')
  }

  const response = await fetch(path, {
    ...init,
    headers,
    credentials: 'include',
  })

  const payload = await parseJSON<T & AdminErrorPayload>(response)
  if (!response.ok) {
    const message = payload?.error?.message || `Request failed with status ${response.status}`
    const error = new AdminApiError(message, response.status, payload?.error?.code)
    if (response.status === 401) {
      markUnauthorized()
    }
    throw error
  }

  return payload as T
}

export function useAdminAuthState() {
  return readonly(adminAuthState)
}

export async function refreshAdminAuthStatus(force = false): Promise<AdminAuthStatus> {
  if (statusPromise && !force) {
    return statusPromise
  }

  adminAuthState.loading = true
  statusPromise = adminRequest<AdminAuthStatus>('/api/admin/status')
    .then((status) => {
      applyStatus(status)
      return status
    })
    .catch((error) => {
      adminAuthState.ready = true
      throw error
    })
    .finally(() => {
      adminAuthState.loading = false
      statusPromise = null
    })

  return statusPromise
}

export async function initializeAdmin(
  username: string,
  password: string,
  setupToken = '',
): Promise<AdminAuthStatus> {
  const status = await adminRequest<AdminAuthStatus>('/api/admin/initialize', {
    method: 'POST',
    body: JSON.stringify({ username, password, setupToken }),
  })
  applyStatus(status)
  return status
}

export async function loginAdmin(username: string, password: string): Promise<AdminAuthStatus> {
  const status = await adminRequest<AdminAuthStatus>('/api/admin/login', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
  })
  applyStatus(status)
  return status
}

export async function logoutAdmin(): Promise<void> {
  try {
    await adminRequest<void>('/api/admin/logout', {
      method: 'POST',
    })
  } catch (error) {
    if (!(error instanceof AdminApiError) || error.status !== 401) {
      throw error
    }
  } finally {
    markUnauthorized()
  }
}

export async function updateAdminCredentials(
  currentPassword: string,
  newUsername: string,
  newPassword: string,
): Promise<AdminAuthStatus> {
  const status = await adminRequest<AdminAuthStatus>('/api/admin/credentials', {
    method: 'POST',
    body: JSON.stringify({
      currentPassword,
      newUsername,
      newPassword,
    }),
  })
  applyStatus(status)
  return status
}

export async function listCodexRelayKeys(): Promise<CodexRelayKeyListItem[]> {
  const response = await adminRequest<{ keys?: CodexRelayKeyListItem[] }>('/api/admin/codex-keys')
  return response.keys ?? []
}

export async function listCodexRelayProviders(): Promise<CodexRelayProviderOption[]> {
  const response = await adminRequest<{ providers?: CodexRelayProviderOption[] }>('/api/admin/codex-providers')
  return response.providers ?? []
}

export async function listClaudeRelayProviders(): Promise<CodexRelayProviderOption[]> {
  const response = await adminRequest<{ providers?: CodexRelayProviderOption[] }>('/api/admin/claude-providers')
  return response.providers ?? []
}

export type GeminiRelayProviderOption = {
  id: string
  name: string
  enabled: boolean
}

export async function listGeminiRelayProviders(): Promise<GeminiRelayProviderOption[]> {
  const response = await adminRequest<{ providers?: GeminiRelayProviderOption[] }>('/api/admin/gemini-providers')
  return response.providers ?? []
}

export async function createCodexRelayKey(
  name: string,
  config?: {
    tokenLimit?: number
    usdLimit?: string
    period?: string
    allowedProviderIds?: number[]
    allowedClaudeProviderIds?: number[]
    allowedGeminiProviderIds?: string[]
  },
): Promise<CodexRelayKeyCreateResult> {
  return adminRequest<CodexRelayKeyCreateResult>('/api/admin/codex-keys', {
    method: 'POST',
    body: JSON.stringify({ name, ...(config ?? {}) }),
  })
}

export async function getCodexRelayKeySecret(id: string): Promise<string> {
  const response = await adminRequest<{ key?: string }>(`/api/admin/codex-keys/${encodeURIComponent(id)}/secret`)
  return response.key ?? ''
}

export async function updateCodexRelayKeyName(id: string, name: string): Promise<string> {
  const response = await adminRequest<{ name?: string }>(`/api/admin/codex-keys/${encodeURIComponent(id)}/name`, {
    method: 'PATCH',
    body: JSON.stringify({ name }),
  })
  return response.name ?? name
}

export async function deleteCodexRelayKey(id: string): Promise<void> {
  await adminRequest<void>(`/api/admin/codex-keys/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  })
}

export async function getCodexRelayKeyQuota(id: string): Promise<CodexRelayQuotaStatus> {
  return adminRequest<CodexRelayQuotaStatus>(`/api/admin/codex-keys/${encodeURIComponent(id)}/quota`)
}

export async function updateCodexRelayKeyQuota(
  id: string,
  quota: { tokenLimit: number; usdLimit: string; period: string },
): Promise<CodexRelayQuotaStatus> {
  return adminRequest<CodexRelayQuotaStatus>(`/api/admin/codex-keys/${encodeURIComponent(id)}/quota`, {
    method: 'PATCH',
    body: JSON.stringify(quota),
  })
}

export async function resetCodexRelayKeyQuota(id: string): Promise<CodexRelayQuotaStatus> {
  return adminRequest<CodexRelayQuotaStatus>(`/api/admin/codex-keys/${encodeURIComponent(id)}/reset-quota`, {
    method: 'POST',
  })
}

export async function updateCodexRelayKeyProviders(id: string, allowedProviderIds: number[]): Promise<number[]> {
  const response = await adminRequest<{ allowedProviderIds?: number[] }>(`/api/admin/codex-keys/${encodeURIComponent(id)}/providers`, {
    method: 'PATCH',
    body: JSON.stringify({ allowedProviderIds }),
  })
  return response.allowedProviderIds ?? []
}

export async function updateCodexRelayKeyClaudeProviders(id: string, allowedClaudeProviderIds: number[]): Promise<number[]> {
  const response = await adminRequest<{ allowedClaudeProviderIds?: number[] }>(`/api/admin/codex-keys/${encodeURIComponent(id)}/claude-providers`, {
    method: 'PATCH',
    body: JSON.stringify({ allowedClaudeProviderIds }),
  })
  return response.allowedClaudeProviderIds ?? []
}

export async function updateCodexRelayKeyGeminiProviders(id: string, allowedGeminiProviderIds: string[]): Promise<string[]> {
  const response = await adminRequest<{ allowedGeminiProviderIds?: string[] }>(`/api/admin/codex-keys/${encodeURIComponent(id)}/gemini-providers`, {
    method: 'PATCH',
    body: JSON.stringify({ allowedGeminiProviderIds }),
  })
  return response.allowedGeminiProviderIds ?? []
}

export async function listProviderKeys(kind: RelayProviderKind): Promise<ProviderKeyItem[]> {
  const response = await adminRequest<{ keys?: ProviderKeyItem[] }>(`/api/admin/provider-keys?kind=${encodeURIComponent(kind)}`)
  return response.keys ?? []
}

// Returns the provider's dedicated key (with secret), creating it on first use.
// Gemini providers are addressed by their string ID.
export async function ensureProviderKey(kind: RelayProviderKind, provider: number | string): Promise<ProviderKeyEnsureResult> {
  const target = kind === 'gemini' ? { providerRef: String(provider) } : { providerId: provider }
  return adminRequest<ProviderKeyEnsureResult>('/api/admin/provider-keys', {
    method: 'POST',
    body: JSON.stringify({ kind, ...target }),
  })
}

// Key used to index dedicated keys per provider: Gemini string ID, otherwise numeric ID.
export const providerKeyRef = (item: { kind: RelayProviderKind; providerId: number; providerRef?: string }): string =>
  item.kind === 'gemini' ? item.providerRef ?? '' : String(item.providerId)

export async function listCodexRelayModelPrices(): Promise<CodexRelayModelPrice[]> {
  const response = await adminRequest<{ prices?: CodexRelayModelPrice[] }>('/api/admin/relay-model-prices')
  return response.prices ?? []
}

export async function upsertCodexRelayModelPrice(price: CodexRelayModelPrice): Promise<CodexRelayModelPrice> {
  return adminRequest<CodexRelayModelPrice>('/api/admin/relay-model-prices', {
    method: 'POST',
    body: JSON.stringify(price),
  })
}

export async function deleteCodexRelayModelPrice(model: string, restoreDefault = false): Promise<void> {
  // Use the query form so model identifiers containing '/' remain routable by
  // Gin's path parameter matcher.
  const url = `/api/admin/relay-model-prices?model=${encodeURIComponent(model)}${restoreDefault ? '&restore=1' : ''}`
  await adminRequest<void>(url, {
    method: 'DELETE',
  })
}

export async function listCodexRelayUnpricedModels(): Promise<CodexRelayUnpricedModel[]> {
  const response = await adminRequest<{ models?: CodexRelayUnpricedModel[] }>('/api/admin/relay-model-prices/unpriced')
  return response.models ?? []
}

if (typeof window !== 'undefined') {
  window.addEventListener(ADMIN_AUTH_INVALID_EVENT, () => {
    markUnauthorized()
  })
}
