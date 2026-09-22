<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { newAPIStates, newAPIAmount, testProviderInfo, infoAmount, infoUnlimited, infoRemaining, type ProviderInfoDraft, type ProviderInfo } from '../../services/providerInfo'
const props = defineProps<{ form: ProviderInfoDraft; open: boolean }>()
const { t } = useI18n()
const busy = ref(false)
const result = ref<ProviderInfo | null>(null)
const failed = ref(false)
let generation = 0
watch(() => [props.open, props.form.apiUrl, props.form.apiKey, props.form.upstreamInfo?.type, props.form.upstreamInfo?.baseUrl], () => {
  generation++; result.value = null; failed.value = false; busy.value = false
})
function selectType(event: Event) {
  const selected = (event.target as HTMLSelectElement).value
  const type = selected === 'sub2api' || selected === 'newapi' ? selected : ''
  props.form.upstreamInfo = { type, baseUrl: props.form.upstreamInfo?.baseUrl || '' }
}
async function test() {
  if (busy.value) return
  const run = ++generation
  busy.value = true; failed.value = false; result.value = null
  try {
    const data = await testProviderInfo({ apiUrl: props.form.apiUrl, apiKey: props.form.apiKey, upstreamInfo: { ...props.form.upstreamInfo! } })
    if (run === generation) result.value = data
  } catch { if (run === generation) failed.value = true }
  finally { if (run === generation) busy.value = false }
}
</script>
<template>
  <fieldset class="info-settings">
    <legend>{{ t('upstreamInfo.title') }}</legend>
    <label class="form-field">
      <span>{{ t('upstreamInfo.service') }}</span>
      <select :value="form.upstreamInfo?.type || ''" @change="selectType">
        <option value="">{{ t('upstreamInfo.off') }}</option><option value="sub2api">sub2api</option><option value="newapi">New API</option>
      </select>
    </label>
    <template v-if="form.upstreamInfo?.type">
      <label class="form-field">
        <span>{{ t('upstreamInfo.baseUrl') }}</span>
        <input v-model="form.upstreamInfo.baseUrl" type="url" placeholder="https://example.com" />
        <span class="field-hint">{{ t('upstreamInfo.baseHint') }}</span>
      </label>
      <button type="button" class="info-button" :disabled="busy || !form.apiKey.trim() || !form.apiUrl.trim()" @click="test">{{ t(busy ? 'upstreamInfo.loading' : 'upstreamInfo.test') }}</button>
      <div aria-live="polite" class="test-result">
        <p v-if="failed">{{ t('upstreamInfo.testFailed') }}</p>
        <template v-if="result?.platform === 'newapi'">
          <p v-for="part in newAPIStates(result)" :key="part.label">{{ t(`upstreamInfo.${part.label}`) }}: {{ t(`upstreamInfo.status.${part.state.status}`) }}<span v-if="part.state.stale"> · {{ t('upstreamInfo.stale') }}</span></p>
          <p v-if="result.key">{{ t('upstreamInfo.keyQuota') }}: {{ result.key.unlimited_quota ? t('upstreamInfo.unlimited') : newAPIAmount(result, 'remaining', t('upstreamInfo.rawUnit')) }}</p>
        </template>
        <template v-else-if="result">
          <p>{{ t('upstreamInfo.usage') }}: {{ t(`upstreamInfo.status.${result.usageState.status}`) }}<span v-if="result.usage"> · {{ t('upstreamInfo.remaining') }} {{ infoUnlimited(result.usage) ? t('upstreamInfo.unlimited') : infoAmount(infoRemaining(result.usage), result.usage.unit || result.usage.quota?.unit) }}</span></p>
          <p>{{ t('upstreamInfo.billing') }}: {{ t(`upstreamInfo.status.${result.billingState.status}`) }}</p>
        </template>
      </div>
      <span class="field-hint">{{ t('upstreamInfo.settingsHint') }}</span>
    </template>
  </fieldset>
</template>
<style scoped>
.info-settings { border: 1px solid var(--mac-border); border-radius: 12px; padding: 14px; display: grid; gap: 12px; min-width: 0; }
legend { padding: 0 6px; font-weight: 600; }
input, select { background: var(--mac-surface); color: var(--mac-text); border: 1px solid var(--mac-border); border-radius: 8px; padding: 9px; width: 100%; box-sizing: border-box; }
.info-button { justify-self: start; background: var(--mac-surface-strong); color: var(--mac-text); border: 1px solid var(--mac-border); padding: 8px 12px; border-radius: 8px; cursor: pointer; }
button:disabled { opacity: .5; cursor: wait; }
.test-result { font-size: 12px; } .test-result p { margin: 4px 0; }
</style>
