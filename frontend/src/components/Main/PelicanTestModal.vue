<template>
  <BaseModal :open="open" :title="t('components.main.pelicanTest.title')" variant="wide" @close="emit('close')">
    <div class="pelican-body">
      <p class="pelican-provider">{{ provider?.name }}</p>
      <label class="pelican-label" for="pelican-model">{{ t('components.main.pelicanTest.model') }}</label>
      <input
        id="pelican-model"
        v-model="model"
        class="pelican-model"
        list="pelican-model-options"
        :disabled="busy"
        :placeholder="t('components.main.pelicanTest.modelPlaceholder')"
        autocomplete="off"
        @keydown.enter="start"
      />
      <datalist id="pelican-model-options">
        <option v-for="option in modelOptions" :key="option" :value="option" />
      </datalist>
      <p v-if="result" class="pelican-meta">{{ t('components.main.pelicanTest.mapping') }}: {{ result.model }} → {{ result.actualModel }}</p>
      <details class="pelican-prompt">
        <summary>{{ t('components.main.pelicanTest.prompt') }}</summary>
        <p>{{ PELICAN_PROMPT }}</p>
      </details>

      <div class="pelican-toolbar">
        <BaseButton :disabled="busy || !model.trim()" @click="start">
          {{ t(result ? 'components.main.pelicanTest.retry' : 'components.main.pelicanTest.start') }}
        </BaseButton>
        <BaseButton v-if="busy" variant="outline" :disabled="!result || cancelling" @click="cancel">
          {{ t('components.main.pelicanTest.cancel') }}
        </BaseButton>
        <span role="status" aria-live="polite">{{ statusLabel }}<template v-if="busy"> · {{ formatTime(elapsedMs) }}</template></span>
      </div>
      <p class="pelican-meta">{{ t('components.main.pelicanTest.usageNotice') }}</p>
      <p v-if="error" class="pelican-error" role="alert">{{ error }}</p>
      <p v-if="result && result.status !== 'running'" class="pelican-meta">
        {{ t('components.main.pelicanTest.firstToken') }}: {{ result.firstTokenMs === null ? '—' : formatTime(result.firstTokenMs) }}
        · {{ t('components.main.pelicanTest.totalTime') }}: {{ formatTime(result.elapsedMs) }}
      </p>

      <div class="pelican-tabs" role="tablist">
        <button v-for="tab in ['preview', 'code'] as const" :key="tab" role="tab" :aria-selected="activeTab === tab" @click="activeTab = tab">
          {{ t(`components.main.pelicanTest.${tab}`) }}
        </button>
      </div>
      <div class="pelican-panels">
        <section class="pelican-panel pelican-preview" :class="{ 'mobile-hidden': activeTab !== 'preview' }">
          <h3>{{ t('components.main.pelicanTest.preview') }}</h3>
          <iframe
            v-if="previewURL && open"
            :key="previewRevision"
            :src="previewURL"
            sandbox="allow-scripts"
            referrerpolicy="no-referrer"
            :title="t('components.main.pelicanTest.preview')"
          />
          <div v-else class="pelican-placeholder">{{ t(busy ? 'components.main.pelicanTest.generating' : 'components.main.pelicanTest.emptyPreview') }}</div>
        </section>
        <section class="pelican-panel pelican-code" :class="{ 'mobile-hidden': activeTab !== 'code' }">
          <h3>{{ t('components.main.pelicanTest.code') }}</h3>
          <pre ref="codeBox">{{ streamText || t('components.main.pelicanTest.waiting') }}</pre>
        </section>
      </div>

      <div class="pelican-toolbar">
        <BaseButton variant="outline" :disabled="!previewURL" @click="previewRevision++">{{ t('components.main.pelicanTest.replay') }}</BaseButton>
        <BaseButton variant="outline" :disabled="!result?.html" @click="copyHTML">{{ t('components.main.pelicanTest.copy') }}</BaseButton>
        <BaseButton variant="outline" :disabled="!result?.html" @click="downloadHTML">{{ t('components.main.pelicanTest.download') }}</BaseButton>
      </div>
      <p class="pelican-meta">{{ t('components.main.pelicanTest.securityNotice') }}</p>
    </div>
  </BaseModal>
</template>

<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseModal from '../common/BaseModal.vue'
import BaseButton from '../common/BaseButton.vue'
import type { AutomationCard } from '../../data/cards'
import { cancelPelicanTest, getPelicanTest, PELICAN_PROMPT, pelicanPreviewURL, startPelicanTest, subscribePelicanProgress, subscribePelicanStream, type PelicanTestResult } from '../../services/pelicanTest'
import { copyText } from '../../utils/clipboard'
import { extractErrorMessage } from '../../utils/error'
import { showToast } from '../../utils/toast'

const props = defineProps<{ open: boolean; platform: string; provider: AutomationCard | null }>()
const emit = defineEmits<{ (event: 'close'): void }>()
const { t } = useI18n()
const model = ref('')
const result = ref<PelicanTestResult | null>(null)
const starting = ref(false)
const cancelling = ref(false)
const error = ref('')
const streamText = ref('')
const streamBytes = ref(0)
const elapsedMs = ref(0)
const activeTab = ref<'preview' | 'code'>('preview')
const previewRevision = ref(0)
const codeBox = ref<HTMLElement | null>(null)
let generation = 0
let pollTimer: number | undefined
let elapsedTimer: number | undefined
let polling = false
let unmounted = false
let unsubscribeStream: (() => void) | undefined
let unsubscribeProgress: (() => void) | undefined
const busy = computed(() => starting.value || result.value?.status === 'running')
const previewURL = computed(() => result.value?.status === 'completed' && result.value.html ? pelicanPreviewURL(result.value) : '')
const modelOptions = computed(() => {
  const provider = props.provider
  const options = new Set([
    ...Object.entries(provider?.supportedModels || {}).filter(([, enabled]) => enabled).map(([name]) => name),
    ...Object.keys(provider?.modelMapping || {}),
    ...Object.values(provider?.modelMapping || {}),
  ])
  return [...options].filter((name) => name && !/[*?]/.test(name)).sort()
})
const statusLabel = computed(() => t(`components.main.pelicanTest.status.${starting.value ? 'running' : result.value?.status || 'idle'}`))
const formatTime = (duration: number) => `${(duration / 1000).toFixed(1)}s`

function stopTimers() {
  window.clearInterval(pollTimer)
  window.clearInterval(elapsedTimer)
  pollTimer = undefined
  elapsedTimer = undefined
}

function reset() {
  const oldSession = result.value?.status === 'running' ? result.value.sessionId : ''
  generation++
  stopTimers()
  result.value = null
  starting.value = false
  cancelling.value = false
  streamText.value = ''
  streamBytes.value = 0
  elapsedMs.value = 0
  error.value = ''
  activeTab.value = 'preview'
  if (oldSession) void cancelPelicanTest(oldSession).catch(() => {})
}

function applyResult(response: PelicanTestResult) {
  result.value = response
  if (response.totalBytes >= streamBytes.value || response.status !== 'running') {
    streamText.value = response.rawOutput
    streamBytes.value = response.totalBytes
  }
  elapsedMs.value = Math.max(elapsedMs.value, response.elapsedMs)
  if (response.status !== 'running') {
    stopTimers()
    cancelling.value = false
    error.value = response.errorCode ? t(`components.main.pelicanTest.errors.${response.errorCode}`, response.message || response.errorCode) : ''
  }
}

async function refresh() {
  const sessionId = result.value?.sessionId
  if (!sessionId || polling) return
  const sequence = generation
  polling = true
  try {
    const response = await getPelicanTest(sessionId)
    if (sequence !== generation || !props.open || unmounted) return
    applyResult(response)
    error.value = response.errorCode ? t(`components.main.pelicanTest.errors.${response.errorCode}`, response.message || response.errorCode) : ''
  } catch (failure) {
    if (sequence === generation && props.open && !unmounted) error.value = extractErrorMessage(failure)
  } finally {
    polling = false
  }
}

async function start() {
  if (busy.value || !model.value.trim() || !props.provider) return
  reset()
  starting.value = true
  const sequence = generation
  const startedAt = Date.now()
  elapsedTimer = window.setInterval(() => { elapsedMs.value = Date.now() - startedAt }, 100)
  try {
    const response = await startPelicanTest(props.platform, props.provider.id, model.value.trim())
    if (sequence !== generation || !props.open || unmounted) {
      void cancelPelicanTest(response.sessionId).catch(() => {})
      return
    }
    applyResult(response)
    pollTimer = window.setInterval(() => { void refresh() }, 1000)
    void refresh()
  } catch (failure) {
    if (sequence !== generation || unmounted) return
    error.value = extractErrorMessage(failure)
    stopTimers()
  } finally {
    if (sequence === generation) starting.value = false
  }
}

async function cancel() {
  if (!result.value || cancelling.value) return
  const sequence = generation
  cancelling.value = true
  try {
    await cancelPelicanTest(result.value.sessionId)
    if (sequence === generation) await refresh()
  } catch (failure) {
    if (sequence === generation) error.value = extractErrorMessage(failure)
  } finally {
    if (sequence === generation) cancelling.value = false
  }
}

async function copyHTML() {
  if (!result.value?.html) return
  try {
    await copyText(result.value.html)
    showToast(t('components.main.pelicanTest.copied'))
  } catch (failure) {
    showToast(extractErrorMessage(failure, t('components.main.pelicanTest.copyFailed')), 'error')
  }
}

function downloadHTML() {
  if (!result.value?.html) return
  const url = URL.createObjectURL(new Blob([result.value.html], { type: 'text/html;charset=utf-8' }))
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = 'pelican-test.html'
  anchor.click()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}

function subscribe() {
  unsubscribeStream = subscribePelicanStream((event) => {
    if (!props.open || event?.sessionId !== result.value?.sessionId || result.value?.status !== 'running') return
    if (event.startBytes !== streamBytes.value) return
    streamText.value += event.chunk
    streamBytes.value = event.totalBytes
    void nextTick(() => {
      const element = codeBox.value
      if (element) element.scrollTop = element.scrollHeight
    })
  })
  unsubscribeProgress = subscribePelicanProgress((event) => {
    if (props.open && event?.sessionId === result.value?.sessionId) void refresh()
  })
}

function unsubscribe() {
  unsubscribeStream?.()
  unsubscribeProgress?.()
  unsubscribeStream = undefined
  unsubscribeProgress = undefined
}

watch(() => props.open, (open) => {
  unsubscribe()
  reset()
  if (open) {
    model.value = modelOptions.value[0] || ''
    subscribe()
  }
}, { immediate: true })

onUnmounted(() => {
  unmounted = true
  reset()
  unsubscribe()
})
</script>

<style scoped>
.pelican-body { display: flex; flex-direction: column; gap: 12px; }
.pelican-provider { font-weight: 600; margin: 0; }
.pelican-label { font-size: 13px; }
.pelican-model { padding: 10px 12px; border: 1px solid #d1d5db; border-radius: 8px; background: transparent; color: inherit; width: 100%; box-sizing: border-box; }
.pelican-meta { font-size: 12px; opacity: .72; margin: 0; overflow-wrap: anywhere; }
.pelican-prompt { font-size: 13px; }
.pelican-prompt summary { cursor: pointer; }
.pelican-prompt p { user-select: text; }
.pelican-toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; font-size: 13px; }
.pelican-error { color: #dc2626; font-size: 13px; margin: 0; }
.pelican-panels { display: grid; grid-template-columns: 1.2fr 1fr; gap: 12px; }
.pelican-panel { min-width: 0; border: 1px solid #d1d5db; border-radius: 10px; overflow: hidden; }
.pelican-panel h3 { padding: 10px 12px; margin: 0; font-size: 13px; border-bottom: 1px solid #d1d5db; }
.pelican-preview iframe { width: 100%; height: 380px; display: block; border: 0; background: white; }
.pelican-placeholder { height: 380px; display: flex; align-items: center; justify-content: center; padding: 20px; box-sizing: border-box; font-size: 13px; opacity: .65; text-align: center; }
.pelican-code pre { height: 380px; margin: 0; padding: 12px; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; box-sizing: border-box; font-size: 12px; user-select: text; }
.pelican-tabs { display: none; }
:global(html.dark) .pelican-model, :global(html.dark) .pelican-panel, :global(html.dark) .pelican-panel h3 { border-color: #475569; }
:global(html.dark) .pelican-error { color: #f87171; }
@media (max-width: 700px) {
  .pelican-panels { grid-template-columns: 1fr; }
  .pelican-tabs { display: flex; gap: 8px; }
  .pelican-tabs button { color: inherit; background: transparent; border: 1px solid #94a3b8; border-radius: 6px; padding: 6px 14px; cursor: pointer; }
  .pelican-tabs button[aria-selected="true"] { background: #2563eb; color: white; border-color: #2563eb; }
  .mobile-hidden { display: none; }
}
</style>
