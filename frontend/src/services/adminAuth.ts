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

export async function createCodexRelayKey(
  name: string,
  quota?: { tokenLimit?: number; usdLimit?: string; period?: string },
): Promise<CodexRelayKeyCreateResult> {
  return adminRequest<CodexRelayKeyCreateResult>('/api/admin/codex-keys', {
    method: 'POST',
    body: JSON.stringify({ name, ...(quota ?? {}) }),
  })
}

export async function getCodexRelayKeySecret(id: string): Promise<string> {
  const response = await adminRequest<{ key?: string }>(`/api/admin/codex-keys/${encodeURIComponent(id)}/secret`)
  return response.key ?? ''
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

export async function deleteCodexRelayModelPrice(model: string): Promise<void> {
  // Use the query form so model identifiers containing '/' remain routable by
  // Gin's path parameter matcher.
  await adminRequest<void>(`/api/admin/relay-model-prices?model=${encodeURIComponent(model)}`, {
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
