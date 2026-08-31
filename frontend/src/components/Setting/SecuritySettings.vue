<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  createCodexRelayKey,
  deleteCodexRelayModelPrice,
  deleteCodexRelayKey,
  getCodexRelayKeyQuota,
  getCodexRelayKeySecret,
  listCodexRelayKeys,
  listCodexRelayModelPrices,
  listCodexRelayProviders,
  listCodexRelayUnpricedModels,
  logoutAdmin,
  resetCodexRelayKeyQuota,
  updateCodexRelayKeyName,
  updateCodexRelayKeyProviders,
  updateCodexRelayKeyQuota,
  upsertCodexRelayModelPrice,
  updateAdminCredentials,
  useAdminAuthState,
  type CodexRelayKeyCreateResult,
  type CodexRelayKeyListItem,
  type CodexRelayModelPrice,
  type CodexRelayProviderOption,
  type CodexRelayUnpricedModel,
} from '../../services/adminAuth'
import { extractErrorMessage } from '../../utils/error'
import { showToast } from '../../utils/toast'

const { t } = useI18n()
const authState = useAdminAuthState()

const currentPassword = ref('')
const newUsername = ref('')
const newPassword = ref('')
const credentialsBusy = ref(false)

const keys = ref<CodexRelayKeyListItem[]>([])
const keysLoading = ref(false)
const keyBusyId = ref('')
const editingNameId = ref('')
const nameBusyId = ref('')
const nameDrafts = ref<Record<string, string>>({})
const createBusy = ref(false)
const createName = ref('')
type QuotaMode = 'usd' | 'token'
type QuotaPeriod = 'once' | 'daily' | 'weekly' | 'monthly'
type QuotaDraft = { mode: QuotaMode; tokenLimit: string; usdLimit: string; period: QuotaPeriod }

const createQuotaMode = ref<QuotaMode>('usd')
const createTokenLimit = ref('0')
const createUsdLimit = ref('0')
const createPeriod = ref<QuotaPeriod>('once')
const createRestrictProviders = ref(false)
const createAllowedProviderIds = ref<number[]>([])
const createdKey = ref<CodexRelayKeyCreateResult | null>(null)
const quotaDrafts = ref<Record<string, QuotaDraft>>({})
const quotaBusyId = ref('')
const quotaRefreshBusyId = ref('')
const providers = ref<CodexRelayProviderOption[]>([])
const providersLoading = ref(false)
const accessDrafts = ref<Record<string, { restricted: boolean; allowedProviderIds: number[] }>>({})
const accessBusyId = ref('')
const modelPrices = ref<CodexRelayModelPrice[]>([])
const unpricedModels = ref<CodexRelayUnpricedModel[]>([])
const pricesLoading = ref(false)
const priceBusyModel = ref('')
const priceDraft = ref<CodexRelayModelPrice>({
  model: '', input: '0', cachedInput: '0', output: '0', reasoningOutput: '0',
})

const formatDateTime = (value: string) => {
  if (!value) {
    return '--'
  }

  const date = new Date(value)
  if (Number.isNaN(date.getTime())) {
    return value
  }
  return date.toLocaleString()
}

const copyToClipboard = async (value: string) => {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value)
    return
  }

  const textArea = document.createElement('textarea')
  textArea.value = value
  textArea.style.position = 'fixed'
  textArea.style.opacity = '0'
  document.body.appendChild(textArea)
  textArea.focus()
  textArea.select()

  const success = document.execCommand('copy')
  document.body.removeChild(textArea)
  if (!success) {
    throw new Error(t('auth.errors.copyFailed'))
  }
}

const loadKeys = async () => {
  keysLoading.value = true
  try {
    keys.value = await listCodexRelayKeys()
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.loadKeysFailed')), 'error')
  } finally {
    keysLoading.value = false
  }
}

const loadProviders = async () => {
  providersLoading.value = true
  try {
    providers.value = await listCodexRelayProviders()
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.loadProvidersFailed')), 'error')
  } finally {
    providersLoading.value = false
  }
}

const draftForKey = (key: CodexRelayKeyListItem) => {
  if (!quotaDrafts.value[key.id]) {
    quotaDrafts.value[key.id] = {
      mode: (key.tokenLimit ?? 0) > 0 && (key.usdLimit || '0') === '0' ? 'token' : 'usd',
      tokenLimit: String(key.tokenLimit ?? 0),
      usdLimit: key.usdLimit || '0',
      period: key.quotaPeriod || 'once',
    }
  }
  return quotaDrafts.value[key.id]
}

const accessDraftForKey = (key: CodexRelayKeyListItem) => {
  if (!accessDrafts.value[key.id]) {
    const allowedProviderIds = [...(key.allowedProviderIds ?? [])]
    accessDrafts.value[key.id] = {
      restricted: allowedProviderIds.length > 0,
      allowedProviderIds,
    }
  }
  return accessDrafts.value[key.id]
}

type ProviderOptionView = CodexRelayProviderOption & { unavailable?: boolean }

const providerOptionsForKey = (key: CodexRelayKeyListItem): ProviderOptionView[] => {
  const knownIDs = new Set(providers.value.map((provider) => provider.id))
  const unavailable = (key.allowedProviderIds ?? [])
    .filter((providerID) => !knownIDs.has(providerID))
    .map((providerID) => ({ id: providerID, name: `#${providerID}`, enabled: false, unavailable: true }))
  return [...providers.value, ...unavailable]
}

const loadPrices = async () => {
  pricesLoading.value = true
  try {
    const [prices, unpriced] = await Promise.all([
      listCodexRelayModelPrices(),
      listCodexRelayUnpricedModels(),
    ])
    modelPrices.value = prices
    unpricedModels.value = unpriced
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.loadPricesFailed')), 'error')
  } finally {
    pricesLoading.value = false
  }
}

const handleUpdateCredentials = async () => {
  if (credentialsBusy.value) {
    return
  }

  credentialsBusy.value = true
  try {
    await updateAdminCredentials(
      currentPassword.value,
      newUsername.value.trim(),
      newPassword.value,
    )
    currentPassword.value = ''
    newUsername.value = ''
    newPassword.value = ''
    showToast(t('auth.security.updateSuccess'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.updateFailed')), 'error')
  } finally {
    credentialsBusy.value = false
  }
}

const handleLogout = async () => {
  if (credentialsBusy.value) {
    return
  }

  credentialsBusy.value = true
  try {
    await logoutAdmin()
    showToast(t('auth.security.logoutSuccess'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.logoutFailed')), 'error')
  } finally {
    credentialsBusy.value = false
  }
}

const handleCreateKey = async () => {
  if (createBusy.value) {
    return
  }

  createBusy.value = true
  try {
    let tokenLimit = 0
    let usdLimit = '0'
    if (createQuotaMode.value === 'token') {
      const tokenText = createTokenLimit.value.trim() || '0'
      tokenLimit = Number.parseInt(tokenText, 10)
      if (!/^\d+$/.test(tokenText) || !Number.isSafeInteger(tokenLimit) || tokenLimit < 0) {
        throw new Error(t('auth.security.invalidTokenLimit'))
      }
    } else {
      usdLimit = createUsdLimit.value.trim() || '0'
      if (!/^\d+(\.\d{1,9})?$/.test(usdLimit)) {
        throw new Error(t('auth.security.invalidUsdLimit'))
      }
    }
    if (createRestrictProviders.value && createAllowedProviderIds.value.length === 0) {
      throw new Error(t('auth.security.selectProviderRequired'))
    }
    createdKey.value = await createCodexRelayKey(createName.value.trim(), {
      tokenLimit,
      usdLimit,
      period: createPeriod.value,
      allowedProviderIds: createRestrictProviders.value ? [...createAllowedProviderIds.value] : [],
    })
    createName.value = ''
    createQuotaMode.value = 'usd'
    createTokenLimit.value = '0'
    createUsdLimit.value = '0'
    createPeriod.value = 'once'
    createRestrictProviders.value = false
    createAllowedProviderIds.value = []
    await loadKeys()
    showToast(t('auth.security.createSuccess'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.createFailed')), 'error')
  } finally {
    createBusy.value = false
  }
}

const handleUpdateProviderAccess = async (key: CodexRelayKeyListItem) => {
  const draft = accessDraftForKey(key)
  if (draft.restricted && draft.allowedProviderIds.length === 0) {
    showToast(t('auth.security.selectProviderRequired'), 'error')
    return
  }
  accessBusyId.value = key.id
  try {
    await updateCodexRelayKeyProviders(key.id, draft.restricted ? [...draft.allowedProviderIds] : [])
    await loadKeys()
    const refreshed = keys.value.find((item) => item.id === key.id)
    const allowedProviderIds = [...(refreshed?.allowedProviderIds ?? [])]
    accessDrafts.value[key.id] = {
      restricted: allowedProviderIds.length > 0,
      allowedProviderIds,
    }
    showToast(t('auth.security.providerAccessUpdated'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.providerAccessUpdateFailed')), 'error')
  } finally {
    accessBusyId.value = ''
  }
}

const handleUpdateQuota = async (key: CodexRelayKeyListItem) => {
  const draft = draftForKey(key)
  let tokenLimit = 0
  let usdLimit = '0'
  if (draft.mode === 'token') {
    const tokenText = draft.tokenLimit.trim() || '0'
    tokenLimit = Number.parseInt(tokenText, 10)
    if (!/^\d+$/.test(tokenText) || !Number.isSafeInteger(tokenLimit) || tokenLimit < 0) {
      showToast(t('auth.security.invalidTokenLimit'), 'error')
      return
    }
  } else {
    usdLimit = draft.usdLimit.trim() || '0'
    if (!/^\d+(\.\d{1,9})?$/.test(usdLimit)) {
      showToast(t('auth.security.invalidUsdLimit'), 'error')
      return
    }
  }
  quotaBusyId.value = key.id
  try {
    await updateCodexRelayKeyQuota(key.id, {
      tokenLimit,
      usdLimit,
      period: draft.period,
    })
    await loadKeys()
    const refreshed = keys.value.find((item) => item.id === key.id)
    if (refreshed) {
      quotaDrafts.value[key.id] = {
        mode: (refreshed.tokenLimit ?? 0) > 0 && (refreshed.usdLimit || '0') === '0' ? 'token' : 'usd',
        tokenLimit: String(refreshed.tokenLimit ?? 0),
        usdLimit: refreshed.usdLimit || '0',
        period: refreshed.quotaPeriod || 'once',
      }
    }
    showToast(t('auth.security.quotaUpdated'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.quotaUpdateFailed')), 'error')
  } finally {
    quotaBusyId.value = ''
  }
}

const handleResetQuota = async (key: CodexRelayKeyListItem) => {
  if (!window.confirm(t('auth.security.resetQuotaConfirm', { name: key.name }))) {
    return
  }
  quotaBusyId.value = key.id
  try {
    await resetCodexRelayKeyQuota(key.id)
    await loadKeys()
    showToast(t('auth.security.quotaReset'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.quotaResetFailed')), 'error')
  } finally {
    quotaBusyId.value = ''
  }
}

const handleRefreshQuota = async (key: CodexRelayKeyListItem) => {
  if (quotaRefreshBusyId.value || quotaBusyId.value === key.id) {
    return
  }
  quotaRefreshBusyId.value = key.id
  try {
    const quota = await getCodexRelayKeyQuota(key.id)
    const index = keys.value.findIndex((item) => item.id === key.id)
    if (index >= 0) {
      keys.value[index] = { ...keys.value[index], quota }
    }
    showToast(t('auth.security.quotaRefreshed'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.quotaRefreshFailed')), 'error')
  } finally {
    quotaRefreshBusyId.value = ''
  }
}

const editPrice = (price: CodexRelayModelPrice) => {
  priceDraft.value = {
    model: price.model,
    input: price.input,
    cachedInput: price.cachedInput,
    output: price.output,
    reasoningOutput: price.reasoningOutput,
  }
}

const clearPriceDraft = () => {
  priceDraft.value = { model: '', input: '0', cachedInput: '0', output: '0', reasoningOutput: '0' }
}

const handleSavePrice = async () => {
  const draft = priceDraft.value
  if (!draft.model.trim() || !['input', 'cachedInput', 'output', 'reasoningOutput'].every((field) => /^\d+(\.\d{1,9})?$/.test(draft[field] || ''))) {
    showToast(t('auth.security.invalidPrice'), 'error')
    return
  }
  priceBusyModel.value = draft.model
  try {
    await upsertCodexRelayModelPrice({ ...draft, model: draft.model.trim() })
    clearPriceDraft()
    await loadPrices()
    showToast(t('auth.security.priceSaved'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.priceSaveFailed')), 'error')
  } finally {
    priceBusyModel.value = ''
  }
}

const handleDeletePrice = async (price: CodexRelayModelPrice) => {
  const restoreDefault = !!price.canRestoreDefault
  const confirmKey = restoreDefault ? 'auth.security.restorePriceConfirm' : 'auth.security.deletePriceConfirm'
  if (!window.confirm(t(confirmKey, { model: price.model }))) {
    return
  }
  priceBusyModel.value = price.model
  try {
    await deleteCodexRelayModelPrice(price.model)
    if (priceDraft.value.model === price.model) clearPriceDraft()
    await loadPrices()
    showToast(t(restoreDefault ? 'auth.security.priceRestored' : 'auth.security.priceDeleted'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.priceDeleteFailed')), 'error')
  } finally {
    priceBusyModel.value = ''
  }
}

const handleCopyCreatedKey = async () => {
  if (!createdKey.value?.key) {
    return
  }

  try {
    await copyToClipboard(createdKey.value.key)
    showToast(t('auth.security.copied'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.errors.copyFailed')), 'error')
  }
}

const handleCopyExistingKey = async (id: string) => {
  keyBusyId.value = id
  try {
    const secret = await getCodexRelayKeySecret(id)
    await copyToClipboard(secret)
    showToast(t('auth.security.copied'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.copyFailed')), 'error')
  } finally {
    keyBusyId.value = ''
  }
}

const beginEditKeyName = (key: CodexRelayKeyListItem) => {
  if (nameBusyId.value) {
    return
  }
  nameDrafts.value[key.id] = key.name
  editingNameId.value = key.id
}

const cancelEditKeyName = (key: CodexRelayKeyListItem) => {
  if (nameBusyId.value === key.id) {
    return
  }
  nameDrafts.value[key.id] = key.name
  if (editingNameId.value === key.id) {
    editingNameId.value = ''
  }
}

const handleUpdateKeyName = async (key: CodexRelayKeyListItem) => {
  const name = (nameDrafts.value[key.id] ?? key.name).trim()
  if (!name) {
    showToast(t('auth.security.invalidKeyName'), 'error')
    return
  }
  if (name === key.name) {
    cancelEditKeyName(key)
    return
  }

  nameBusyId.value = key.id
  try {
    const updatedName = await updateCodexRelayKeyName(key.id, name)
    const index = keys.value.findIndex((item) => item.id === key.id)
    if (index >= 0) {
      keys.value[index] = { ...keys.value[index], name: updatedName }
    }
    if (createdKey.value?.id === key.id) {
      createdKey.value = { ...createdKey.value, name: updatedName }
    }
    nameDrafts.value[key.id] = updatedName
    editingNameId.value = ''
    showToast(t('auth.security.keyNameUpdated'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.keyNameUpdateFailed')), 'error')
  } finally {
    nameBusyId.value = ''
  }
}

const handleDeleteKey = async (key: CodexRelayKeyListItem) => {
  if (!window.confirm(t('auth.security.deleteConfirm', { name: key.name }))) {
    return
  }

  keyBusyId.value = key.id
  try {
    await deleteCodexRelayKey(key.id)
    if (createdKey.value?.id === key.id) {
      createdKey.value = null
    }
    await loadKeys()
    showToast(t('auth.security.deleteSuccess'), 'success')
  } catch (error) {
    showToast(extractErrorMessage(error, t('auth.security.deleteFailed')), 'error')
  } finally {
    keyBusyId.value = ''
  }
}

onMounted(async () => {
  await Promise.all([loadKeys(), loadProviders(), loadPrices()])
})
</script>

<template>
  <section>
    <h2 class="mac-section-title">{{ t('components.general.title.security') }}</h2>
    <p class="mac-section-description">{{ t('auth.security.description') }}</p>

    <div class="mac-panel security-card">
      <div class="security-card-header">
        <div>
          <h3 class="security-card-title">{{ t('auth.security.adminCardTitle') }}</h3>
          <p class="security-card-description">
            {{ t('auth.security.adminCardDescription', { username: authState.username || '--' }) }}
          </p>
        </div>
        <span class="security-badge">{{ authState.username || '--' }}</span>
      </div>

      <div class="security-grid">
        <label class="security-field">
          <span>{{ t('auth.fields.currentPassword') }}</span>
          <input
            v-model="currentPassword"
            class="base-input"
            type="password"
            autocomplete="current-password"
            :placeholder="t('auth.placeholders.currentPassword')"
            :disabled="credentialsBusy"
          />
          <small class="security-field-placeholder" aria-hidden="true">&nbsp;</small>
        </label>

        <label class="security-field">
          <span>{{ t('auth.fields.newUsername') }}</span>
          <input
            v-model="newUsername"
            class="base-input"
            type="text"
            autocomplete="username"
            :placeholder="t('auth.placeholders.newUsername')"
            :disabled="credentialsBusy"
          />
          <small>{{ t('auth.security.keepHint') }}</small>
        </label>

        <label class="security-field">
          <span>{{ t('auth.fields.newPassword') }}</span>
          <input
            v-model="newPassword"
            class="base-input"
            type="password"
            autocomplete="new-password"
            :placeholder="t('auth.placeholders.newPassword')"
            :disabled="credentialsBusy"
          />
          <small>{{ t('auth.security.keepHint') }}</small>
        </label>
      </div>

      <div class="security-actions">
        <button
          class="security-btn"
          :disabled="credentialsBusy"
          @click="handleUpdateCredentials"
        >
          {{ credentialsBusy ? t('auth.security.updating') : t('auth.security.update') }}
        </button>
        <button
          class="security-btn secondary"
          :disabled="credentialsBusy"
          @click="handleLogout"
        >
          {{ t('auth.security.logout') }}
        </button>
      </div>
    </div>

    <div class="mac-panel security-card">
      <div class="security-card-header">
        <div>
          <h3 class="security-card-title">{{ t('auth.security.keysCardTitle') }}</h3>
          <p class="security-card-description">{{ t('auth.security.keysCardDescription') }}</p>
        </div>
      </div>

      <div class="security-create-row">
        <label class="security-field security-field-grow">
          <span>{{ t('auth.security.createLabel') }}</span>
          <input
            v-model="createName"
            class="base-input"
            type="text"
            maxlength="128"
            :placeholder="t('auth.security.createPlaceholder')"
            :disabled="createBusy"
            @keyup.enter="handleCreateKey"
          />
        </label>
        <fieldset class="security-field quota-mode-field">
          <legend>{{ t('auth.security.quotaType') }}</legend>
          <div class="quota-mode-toggle" role="radiogroup" :aria-label="t('auth.security.quotaType')">
            <label :class="{ active: createQuotaMode === 'usd' }">
              <input v-model="createQuotaMode" type="radio" value="usd" :disabled="createBusy" />
              <span>{{ t('auth.security.quotaTypeUsd') }}</span>
            </label>
            <label :class="{ active: createQuotaMode === 'token' }">
              <input v-model="createQuotaMode" type="radio" value="token" :disabled="createBusy" />
              <span>{{ t('auth.security.quotaTypeToken') }}</span>
            </label>
          </div>
        </fieldset>
        <label v-if="createQuotaMode === 'usd'" class="security-field quota-create-field">
          <span>{{ t('auth.security.usdLimit') }}</span>
          <input v-model="createUsdLimit" class="base-input" inputmode="decimal" :disabled="createBusy" />
        </label>
        <label v-else class="security-field quota-create-field">
          <span>{{ t('auth.security.tokenLimit') }}</span>
          <input v-model="createTokenLimit" class="base-input" type="number" min="0" step="1" :disabled="createBusy" />
        </label>
        <label class="security-field quota-create-field">
          <span>{{ t('auth.security.period') }}</span>
          <select v-model="createPeriod" class="base-input" :disabled="createBusy">
            <option value="once">{{ t('auth.security.periodOnce') }}</option>
            <option value="daily">{{ t('auth.security.periodDaily') }}</option>
            <option value="weekly">{{ t('auth.security.periodWeekly') }}</option>
            <option value="monthly">{{ t('auth.security.periodMonthly') }}</option>
          </select>
        </label>
        <button class="security-btn" :disabled="createBusy" @click="handleCreateKey">
          {{ createBusy ? t('auth.security.creating') : t('auth.security.create') }}
        </button>
      </div>

      <fieldset class="provider-access-control" :aria-label="t('auth.security.providerAccess')">
        <div class="provider-access-header">
          <label class="provider-access-toggle">
            <input
              v-model="createRestrictProviders"
              type="checkbox"
              :disabled="createBusy || providersLoading || providers.length === 0"
            />
            <span>{{ t('auth.security.restrictProviders') }}</span>
          </label>
          <span class="provider-access-state">
            {{ createRestrictProviders ? t('auth.security.selectedProviders') : t('auth.security.allProviders') }}
          </span>
        </div>
        <div v-if="createRestrictProviders" class="provider-options">
          <label v-for="provider in providers" :key="provider.id" class="provider-option">
            <input v-model="createAllowedProviderIds" type="checkbox" :value="provider.id" :disabled="createBusy" />
            <span>{{ provider.name }}</span>
            <small v-if="!provider.enabled">{{ t('auth.security.providerDisabled') }}</small>
          </label>
        </div>
        <span v-else-if="providersLoading" class="provider-access-empty">{{ t('auth.security.loadingProviders') }}</span>
        <span v-else-if="providers.length === 0" class="provider-access-empty">{{ t('auth.security.noProviders') }}</span>
      </fieldset>

      <div v-if="createdKey" class="security-created">
        <div class="security-created-header">
          <div>
            <h4>{{ t('auth.security.oneTimeTitle') }}</h4>
            <p>{{ t('auth.security.oneTimeDescription') }}</p>
          </div>
          <button class="security-btn secondary" @click="handleCopyCreatedKey">
            {{ t('auth.security.copy') }}
          </button>
        </div>
        <code class="security-secret">{{ createdKey.key }}</code>
      </div>

      <div v-if="keysLoading" class="security-empty">
        {{ t('auth.security.loadingKeys') }}
      </div>
      <div v-else-if="keys.length === 0" class="security-empty">
        {{ t('auth.security.empty') }}
      </div>
      <div v-else class="security-key-list">
        <article v-for="key in keys" :key="key.id" class="security-key-row">
          <div class="security-key-meta">
            <div v-if="editingNameId === key.id" class="key-name-editor">
              <input
                v-model="nameDrafts[key.id]"
                class="base-input"
                type="text"
                maxlength="128"
                autofocus
                :aria-label="t('auth.security.keyName')"
                :disabled="nameBusyId === key.id"
                @keyup.enter="handleUpdateKeyName(key)"
                @keyup.esc="cancelEditKeyName(key)"
              />
              <button class="security-btn secondary key-name-button" :disabled="nameBusyId === key.id" @click="handleUpdateKeyName(key)">
                {{ nameBusyId === key.id ? t('auth.security.savingKeyName') : t('auth.security.saveKeyName') }}
              </button>
              <button class="security-btn secondary key-name-button" :disabled="nameBusyId === key.id" @click="cancelEditKeyName(key)">
                {{ t('common.cancel') }}
              </button>
            </div>
            <div v-else class="key-name-display">
              <strong>{{ key.name }}</strong>
              <button class="security-btn secondary key-name-button" :disabled="keyBusyId === key.id || nameBusyId !== ''" @click="beginEditKeyName(key)">
                {{ t('auth.security.editKeyName') }}
              </button>
            </div>
            <span>{{ formatDateTime(key.createdAt) }}</span>
          </div>
          <code class="security-key-value">{{ key.maskedKey }}</code>
          <div class="security-key-actions">
            <button
              class="security-btn secondary"
              :disabled="keyBusyId === key.id || nameBusyId === key.id"
              @click="handleCopyExistingKey(key.id)"
            >
              {{ t('auth.security.copy') }}
            </button>
            <button
              class="security-btn danger"
              :disabled="keyBusyId === key.id || nameBusyId === key.id"
              @click="handleDeleteKey(key)"
            >
              {{ t('auth.security.delete') }}
            </button>
          </div>
          <div class="quota-editor">
            <div class="quota-summary">
              <span v-if="draftForKey(key).mode === 'token'">{{ t('auth.security.tokenUsage') }}: {{ key.quota?.tokenUsed ?? 0 }} / {{ key.tokenLimit || t('auth.security.unlimited') }}</span>
              <span v-else>{{ t('auth.security.usdUsage') }}: ${{ key.quota?.usdUsed ?? '0' }} / {{ key.usdLimit === '0' ? t('auth.security.unlimited') : `$${key.usdLimit}` }}</span>
              <span>{{ t('auth.security.period') }}: {{ t(`auth.security.period${(key.quotaPeriod || 'once').charAt(0).toUpperCase()}${(key.quotaPeriod || 'once').slice(1)}`) }}</span>
              <span v-if="key.quota?.resetAt">{{ t('auth.security.nextReset') }}: {{ formatDateTime(key.quota.resetAt) }} ({{ key.quota.serverTimezone }})</span>
              <strong v-if="key.quota?.blocked" class="quota-blocked">{{ t('auth.security.quotaBlocked') }}</strong>
            </div>
            <div class="quota-edit-fields">
              <div class="quota-mode-toggle" role="radiogroup" :aria-label="t('auth.security.quotaType')">
                <label :class="{ active: draftForKey(key).mode === 'usd' }">
                  <input v-model="draftForKey(key).mode" type="radio" value="usd" />
                  <span>{{ t('auth.security.quotaTypeUsd') }}</span>
                </label>
                <label :class="{ active: draftForKey(key).mode === 'token' }">
                  <input v-model="draftForKey(key).mode" type="radio" value="token" />
                  <span>{{ t('auth.security.quotaTypeToken') }}</span>
                </label>
              </div>
              <input v-if="draftForKey(key).mode === 'token'" v-model="draftForKey(key).tokenLimit" class="base-input" type="number" min="0" step="1" :aria-label="t('auth.security.tokenLimit')" />
              <input v-else v-model="draftForKey(key).usdLimit" class="base-input" inputmode="decimal" :aria-label="t('auth.security.usdLimit')" />
              <select v-model="draftForKey(key).period" class="base-input" :aria-label="t('auth.security.period')">
                <option value="once">{{ t('auth.security.periodOnce') }}</option>
                <option value="daily">{{ t('auth.security.periodDaily') }}</option>
                <option value="weekly">{{ t('auth.security.periodWeekly') }}</option>
                <option value="monthly">{{ t('auth.security.periodMonthly') }}</option>
              </select>
              <button
                class="security-btn secondary"
                :disabled="quotaBusyId === key.id || quotaRefreshBusyId !== ''"
                @click="handleRefreshQuota(key)"
              >
                {{ quotaRefreshBusyId === key.id ? t('auth.security.refreshingQuota') : t('auth.security.refreshQuota') }}
              </button>
              <button class="security-btn secondary" :disabled="quotaBusyId === key.id || quotaRefreshBusyId === key.id" @click="handleUpdateQuota(key)">{{ t('auth.security.saveQuota') }}</button>
              <button class="security-btn secondary" :disabled="quotaBusyId === key.id || quotaRefreshBusyId === key.id" @click="handleResetQuota(key)">{{ t('auth.security.resetQuota') }}</button>
            </div>
          </div>
          <fieldset class="provider-access-editor" :aria-label="t('auth.security.providerAccess')">
            <div class="provider-access-header">
              <label class="provider-access-toggle">
                <input
                  v-model="accessDraftForKey(key).restricted"
                  type="checkbox"
                  :disabled="accessBusyId === key.id"
                />
                <span>{{ t('auth.security.restrictProviders') }}</span>
              </label>
              <span class="provider-access-state">
                {{ accessDraftForKey(key).restricted ? t('auth.security.selectedProviders') : t('auth.security.allProviders') }}
              </span>
            </div>
            <div v-if="accessDraftForKey(key).restricted" class="provider-options">
              <label v-for="provider in providerOptionsForKey(key)" :key="provider.id" class="provider-option">
                <input
                  v-model="accessDraftForKey(key).allowedProviderIds"
                  type="checkbox"
                  :value="provider.id"
                  :disabled="accessBusyId === key.id"
                />
                <span>{{ provider.name }}</span>
                <small v-if="provider.unavailable">{{ t('auth.security.providerUnavailable') }}</small>
                <small v-else-if="!provider.enabled">{{ t('auth.security.providerDisabled') }}</small>
              </label>
              <span v-if="providerOptionsForKey(key).length === 0" class="provider-access-empty">{{ t('auth.security.noProviders') }}</span>
            </div>
            <div class="provider-access-actions">
              <button
                class="security-btn secondary"
                :disabled="accessBusyId === key.id"
                @click="handleUpdateProviderAccess(key)"
              >
                {{ t('auth.security.saveProviderAccess') }}
              </button>
            </div>
          </fieldset>
        </article>
      </div>
    </div>

    <div class="mac-panel security-card">
      <div class="security-card-header">
        <div>
          <h3 class="security-card-title">{{ t('auth.security.pricesTitle') }}</h3>
          <p class="security-card-description">{{ t('auth.security.pricesDescription') }}</p>
        </div>
      </div>
      <div v-if="unpricedModels.length" class="unpriced-warning">
        {{ t('auth.security.unpricedWarning', { count: unpricedModels.length }) }}
        <span v-for="model in unpricedModels" :key="model.model" class="unpriced-model">{{ model.model }}</span>
      </div>
      <div class="price-editor">
        <input v-model="priceDraft.model" class="base-input" :placeholder="t('auth.security.modelName')" />
        <input v-model="priceDraft.input" class="base-input" inputmode="decimal" :placeholder="t('auth.security.inputPrice')" />
        <input v-model="priceDraft.cachedInput" class="base-input" inputmode="decimal" :placeholder="t('auth.security.cachedInputPrice')" />
        <input v-model="priceDraft.output" class="base-input" inputmode="decimal" :placeholder="t('auth.security.outputPrice')" />
        <input v-model="priceDraft.reasoningOutput" class="base-input" inputmode="decimal" :placeholder="t('auth.security.reasoningPrice')" />
        <button class="security-btn" :disabled="pricesLoading" @click="handleSavePrice">{{ t('auth.security.savePrice') }}</button>
        <button class="security-btn secondary" :disabled="pricesLoading" @click="clearPriceDraft">{{ t('common.cancel') }}</button>
      </div>
      <div v-if="pricesLoading" class="security-empty">{{ t('auth.security.loadingPrices') }}</div>
      <div v-else-if="modelPrices.length === 0" class="security-empty">{{ t('auth.security.emptyPrices') }}</div>
      <div v-else class="price-list">
        <article v-for="price in modelPrices" :key="price.model" class="price-row">
          <div class="price-model">
            <strong>{{ price.model }}</strong>
            <span class="price-source" :class="price.source">{{ t(price.source === 'custom' ? 'auth.security.priceSourceCustom' : 'auth.security.priceSourceBuiltin') }}</span>
          </div>
          <span>{{ t('auth.security.inputShort') }} {{ price.input }}</span>
          <span>{{ t('auth.security.cachedShort') }} {{ price.cachedInput }}</span>
          <span>{{ t('auth.security.outputShort') }} {{ price.output }}</span>
          <span>{{ t('auth.security.reasoningShort') }} {{ price.reasoningOutput }}</span>
          <div class="security-key-actions">
            <button class="security-btn secondary" @click="editPrice(price)">{{ t('auth.security.editPrice') }}</button>
            <button v-if="price.source === 'custom'" class="security-btn danger" :disabled="priceBusyModel === price.model" @click="handleDeletePrice(price)">{{ t(price.canRestoreDefault ? 'auth.security.restorePrice' : 'auth.security.deletePrice') }}</button>
          </div>
        </article>
      </div>
    </div>
  </section>
</template>

<style scoped>
.security-card {
  padding: 22px;
  display: grid;
  gap: 20px;
}

.security-card + .security-card {
  margin-top: 14px;
}

.security-card-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.security-card-title {
  margin: 0;
  font-size: 1rem;
}

.security-card-description {
  margin: 6px 0 0;
  color: var(--mac-text-secondary);
  line-height: 1.6;
}

.security-badge {
  display: inline-flex;
  align-items: center;
  min-height: 34px;
  padding: 0 14px;
  border-radius: 999px;
  background: color-mix(in srgb, var(--mac-accent) 12%, var(--mac-surface));
  color: var(--mac-text);
  font-size: 0.88rem;
  font-weight: 700;
}

.security-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 14px;
}

.security-field {
  display: grid;
  gap: 8px;
}

.security-field .base-input {
  height: 42px;
  box-sizing: border-box;
}

.security-field select.base-input {
  padding-top: 0;
  padding-bottom: 0;
  line-height: normal;
}

.security-field span {
  font-size: 0.9rem;
  font-weight: 600;
}

.security-field small {
  color: var(--mac-text-secondary);
  font-size: 0.76rem;
  min-height: 16px;
  line-height: 1.35;
}

.security-field-placeholder {
  visibility: hidden;
}

.security-actions,
.security-create-row,
.security-key-actions,
.security-created-header {
  display: flex;
  align-items: center;
  gap: 12px;
}

.security-actions {
  justify-content: flex-end;
}

.security-create-row {
  align-items: flex-end;
  flex-wrap: wrap;
}

.security-field-grow {
  flex: 1;
  min-width: 180px;
}

.quota-create-field {
  min-width: 130px;
}

.quota-mode-field {
  min-width: 176px;
  margin: 0;
  padding: 0;
  border: 0;
}

.quota-mode-field legend {
  margin-bottom: 8px;
  padding: 0;
  font-size: 0.9rem;
  font-weight: 600;
}

.quota-mode-toggle {
  display: inline-grid;
  grid-template-columns: repeat(2, minmax(70px, 1fr));
  align-items: stretch;
  min-height: 38px;
  box-sizing: border-box;
  padding: 3px;
  border: 1px solid var(--mac-border);
  border-radius: 7px;
  background: color-mix(in srgb, var(--mac-text) 5%, var(--mac-surface));
}

.quota-mode-field .quota-mode-toggle {
  min-height: 42px;
}

.quota-mode-toggle label {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  min-width: 0;
  padding: 0 10px;
  border-radius: 5px;
  color: var(--mac-text-secondary);
  cursor: pointer;
  font-size: 0.82rem;
  font-weight: 700;
}

.quota-mode-toggle label.active {
  background: var(--mac-surface);
  color: var(--mac-text);
  box-shadow: 0 1px 3px color-mix(in srgb, #000 16%, transparent);
}

.quota-mode-toggle label:has(input:focus-visible) {
  outline: 2px solid var(--mac-accent);
  outline-offset: -2px;
}

.quota-mode-toggle input {
  position: absolute;
  width: 1px;
  height: 1px;
  opacity: 0;
  pointer-events: none;
}

.security-btn {
  min-height: 42px;
  border: none;
  border-radius: 14px;
  padding: 0 16px;
  background: linear-gradient(135deg, #0a84ff 0%, #1271d5 100%);
  color: #fff;
  font-weight: 700;
  cursor: pointer;
  transition: opacity 0.18s ease, transform 0.18s ease;
}

.security-btn:hover:not(:disabled) {
  transform: translateY(-1px);
}

.security-btn:disabled {
  opacity: 0.65;
  cursor: wait;
}

.security-btn.secondary {
  background: color-mix(in srgb, var(--mac-text) 12%, var(--mac-surface));
  color: var(--mac-text);
}

.security-btn.danger {
  background: linear-gradient(135deg, #f43f5e 0%, #e11d48 100%);
}

.security-created {
  display: grid;
  gap: 12px;
  padding: 16px;
  border-radius: 18px;
  background: color-mix(in srgb, var(--mac-accent) 8%, var(--mac-surface));
  border: 1px solid color-mix(in srgb, var(--mac-accent) 18%, transparent);
}

.security-created-header {
  justify-content: space-between;
}

.security-created-header h4 {
  margin: 0;
}

.security-created-header p {
  margin: 6px 0 0;
  color: var(--mac-text-secondary);
}

.security-secret,
.security-key-value {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 0.86rem;
  word-break: break-all;
}

.security-secret {
  display: block;
  padding: 14px 16px;
  border-radius: 16px;
  background: color-mix(in srgb, var(--mac-surface-strong) 86%, transparent);
}

.security-key-list {
  display: grid;
  gap: 12px;
}

.security-key-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(200px, 0.9fr) auto;
  align-items: center;
  gap: 16px;
  padding: 14px 16px;
  border-radius: 18px;
  background: color-mix(in srgb, var(--mac-surface-strong) 82%, transparent);
}

.quota-editor {
  grid-column: 1 / -1;
  display: grid;
  gap: 10px;
  padding-top: 10px;
  border-top: 1px solid color-mix(in srgb, var(--mac-border) 70%, transparent);
}

.provider-access-control,
.provider-access-editor {
  min-width: 0;
  margin: 0;
  border: 0;
}

.provider-access-control {
  padding: 12px 0 0;
  border-top: 1px solid color-mix(in srgb, var(--mac-border) 70%, transparent);
}

.provider-access-editor {
  grid-column: 1 / -1;
  display: grid;
  gap: 10px;
  padding: 10px 0 0;
  border-top: 1px solid color-mix(in srgb, var(--mac-border) 70%, transparent);
}

.provider-access-header,
.provider-access-toggle,
.provider-access-actions {
  display: flex;
  align-items: center;
}

.provider-access-header {
  justify-content: space-between;
  gap: 12px;
}

.provider-access-toggle {
  gap: 8px;
  font-size: 0.88rem;
  font-weight: 700;
}

.provider-access-toggle input,
.provider-option input {
  width: 16px;
  height: 16px;
  margin: 0;
  accent-color: var(--mac-accent);
}

.provider-access-state {
  padding: 3px 8px;
  border-radius: 6px;
  background: color-mix(in srgb, var(--mac-text) 10%, var(--mac-surface));
  color: var(--mac-text-secondary);
  font-size: 0.76rem;
  font-weight: 700;
}

.provider-options {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
  gap: 8px;
}

.provider-option {
  display: grid;
  grid-template-columns: 16px minmax(0, 1fr) auto;
  align-items: center;
  gap: 8px;
  min-height: 36px;
  padding: 6px 9px;
  border: 1px solid color-mix(in srgb, var(--mac-border) 75%, transparent);
  border-radius: 6px;
  color: var(--mac-text);
  font-size: 0.82rem;
}

.provider-option span {
  min-width: 0;
  overflow-wrap: anywhere;
}

.provider-option small,
.provider-access-empty {
  color: var(--mac-text-secondary);
  font-size: 0.74rem;
}

.provider-access-actions {
  justify-content: flex-end;
}

.provider-access-actions .security-btn {
  min-height: 36px;
  border-radius: 6px;
  padding: 0 12px;
}

.quota-summary,
.quota-edit-fields,
.price-editor,
.price-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.quota-summary {
  color: var(--mac-text-secondary);
  font-size: 0.8rem;
}

.quota-edit-fields .base-input {
  min-width: 120px;
  height: 38px;
  box-sizing: border-box;
  padding-top: 0;
  padding-bottom: 0;
  line-height: normal;
}

.quota-edit-fields .security-btn,
.price-editor .security-btn {
  min-height: 36px;
  border-radius: 10px;
  padding: 0 12px;
}

.quota-blocked {
  color: #d92d20;
}

.prices-title {
  margin-top: 14px;
}

.price-editor .base-input {
  flex: 1 1 130px;
  min-width: 110px;
  height: 38px;
}

.price-list {
  display: grid;
  gap: 10px;
}

.price-row {
  justify-content: space-between;
  padding: 12px 14px;
  border-radius: 12px;
  background: color-mix(in srgb, var(--mac-surface-strong) 82%, transparent);
  color: var(--mac-text-secondary);
  font-size: 0.82rem;
}

.price-row strong {
  color: var(--mac-text);
  min-width: 180px;
}

.price-model {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 230px;
}

.price-source {
  flex: 0 0 auto;
  padding: 2px 6px;
  border-radius: 5px;
  background: color-mix(in srgb, var(--mac-text) 9%, transparent);
  color: var(--mac-text-secondary);
  font-size: 0.7rem;
  font-weight: 700;
}

.price-source.custom {
  background: color-mix(in srgb, #0a84ff 14%, transparent);
  color: #0a6ed1;
}

.unpriced-warning {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  padding: 10px 12px;
  border: 1px solid color-mix(in srgb, #d97706 35%, transparent);
  border-radius: 10px;
  background: color-mix(in srgb, #f59e0b 12%, var(--mac-surface));
  color: var(--mac-text);
  font-size: 0.82rem;
}

.unpriced-model {
  padding: 2px 7px;
  border-radius: 6px;
  background: color-mix(in srgb, #f59e0b 20%, transparent);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}

.security-key-meta {
  display: grid;
  gap: 4px;
}

.key-name-display,
.key-name-editor {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  flex-wrap: wrap;
}

.key-name-display strong {
  min-width: 0;
  overflow-wrap: anywhere;
}

.key-name-editor .base-input {
  width: min(240px, 100%);
  height: 34px;
  box-sizing: border-box;
  padding: 0 10px;
}

.key-name-button {
  min-height: 32px;
  padding: 0 10px;
  border-radius: 8px;
  font-size: 0.76rem;
}

.security-key-meta span {
  color: var(--mac-text-secondary);
  font-size: 0.82rem;
}

.security-empty {
  padding: 16px;
  border-radius: 18px;
  background: color-mix(in srgb, var(--mac-surface-strong) 82%, transparent);
  color: var(--mac-text-secondary);
}

@media (max-width: 900px) {
  .security-grid,
  .security-key-row {
    grid-template-columns: 1fr;
  }

  .security-card-header,
  .security-created-header,
  .security-create-row,
  .security-key-actions,
  .security-actions {
    flex-direction: column;
    align-items: stretch;
  }

  .provider-access-header,
  .provider-access-actions {
    align-items: stretch;
    flex-direction: column;
  }

  .security-badge {
    width: fit-content;
  }
}
</style>
