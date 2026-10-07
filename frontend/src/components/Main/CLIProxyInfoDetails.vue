<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseModal from '../common/BaseModal.vue'
import { infoTime, type ProviderInfo } from '../../services/providerInfo'
const props = defineProps<{ open: boolean; name: string; data: ProviderInfo | null; busy: boolean; failed: boolean }>()
defineEmits<{ (e: 'close'): void; (e: 'refresh'): void }>()
const { t } = useI18n()
const search = ref('')
const rows = computed(() => props.data?.models?.data || [])
const filtered = computed(() => rows.value.filter(row => row.id.toLowerCase().includes(search.value.trim().toLowerCase())))
const state = computed(() => props.data?.modelsState)
watch(() => props.open, open => { if (open) search.value = '' })
</script>
<template>
  <BaseModal :open="open" :title="`${name} · CLIProxyAPI`" @close="$emit('close')">
    <div class="cliproxy-detail">
      <div class="toolbar"><p class="hint">{{ t('upstreamInfo.cliproxyScope') }}</p><button type="button" :disabled="busy" @click="$emit('refresh')">{{ t(busy ? 'upstreamInfo.loading' : 'upstreamInfo.refresh') }}</button></div>
      <p v-if="failed" role="status">{{ t('upstreamInfo.fetchFailed') }}</p>
      <p v-if="state" aria-live="polite">{{ t('upstreamInfo.availableModels') }} · {{ t(`upstreamInfo.status.${state.status}`) }}<span v-if="state.stale"> · {{ t('upstreamInfo.stale') }}</span><span v-if="state.updatedAt"> · {{ infoTime(state.updatedAt) }}</span><span v-if="state.retryAt"> · {{ t('upstreamInfo.retryAt') }} {{ infoTime(state.retryAt) }}</span></p>
      <p v-else aria-live="polite">{{ t(busy ? 'upstreamInfo.loading' : 'upstreamInfo.pending') }}</p>
      <template v-if="data?.models">
        <div class="toolbar"><h3>{{ t('upstreamInfo.availableModels') }} ({{ rows.length }})</h3><input v-model="search" type="search" :aria-label="t('upstreamInfo.searchModel')" :placeholder="t('upstreamInfo.searchModel')" /></div>
        <div class="table-scroll">
          <table><thead><tr><th>{{ t('upstreamInfo.model') }}</th><th>{{ t('upstreamInfo.modelOwner') }}</th></tr></thead><tbody><tr v-for="row in filtered" :key="row.id"><td>{{ row.id }}</td><td>{{ row.owned_by || '—' }}</td></tr></tbody></table>
          <p v-if="!filtered.length" class="hint">{{ t('upstreamInfo.noModels') }}</p>
        </div>
      </template>
    </div>
  </BaseModal>
</template>
<style scoped>
.cliproxy-detail { color: var(--mac-text); font-size: 13px; }.toolbar { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 12px; }.hint { color: var(--mac-text-secondary); line-height: 1.6; flex: 1; }h3 { font-size: 14px; margin: 0; }
button { border: 0; background: transparent; color: var(--mac-accent, #0a84ff); cursor: pointer; white-space: nowrap; }button:disabled { opacity: .5; cursor: wait; }input { border: 1px solid var(--mac-border); border-radius: 6px; padding: 7px 10px; background: var(--mac-surface); color: var(--mac-text); max-width: 100%; }
.table-scroll { overflow: auto; max-height: 400px; }table { border-collapse: collapse; width: 100%; font-size: 12px; }th, td { padding: 9px 10px; text-align: left; border-bottom: 1px solid var(--mac-border); overflow-wrap: anywhere; }th { background: var(--mac-surface-strong); }
</style>
