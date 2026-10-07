<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseModal from '../common/BaseModal.vue'
import { infoTime, type ProviderInfo } from '../../services/providerInfo'
const props = defineProps<{ open: boolean; name: string; data: ProviderInfo | null; busy: boolean; failed: boolean }>()
defineEmits<{ (e: 'close'): void; (e: 'refresh'): void }>()
const { t } = useI18n()
const balance = computed(() => props.data?.balance)
const state = computed(() => props.data?.balanceState)
</script>
<template>
  <BaseModal :open="open" :title="`${name} · DeepSeek`" @close="$emit('close')">
    <div class="deepseek-detail">
      <div class="toolbar"><p class="hint">{{ t('upstreamInfo.deepseekScope') }}</p><button type="button" :disabled="busy" @click="$emit('refresh')">{{ t(busy ? 'upstreamInfo.loading' : 'upstreamInfo.refresh') }}</button></div>
      <p v-if="failed" role="status">{{ t('upstreamInfo.fetchFailed') }}</p>
      <p v-if="state" aria-live="polite">{{ t('upstreamInfo.wallet') }} · {{ t(`upstreamInfo.status.${state.status}`) }}<span v-if="state.stale"> · {{ t('upstreamInfo.stale') }}</span><span v-if="state.updatedAt"> · {{ infoTime(state.updatedAt) }}</span><span v-if="state.retryAt"> · {{ t('upstreamInfo.retryAt') }} {{ infoTime(state.retryAt) }}</span></p>
      <p v-else aria-live="polite">{{ t(busy ? 'upstreamInfo.loading' : 'upstreamInfo.pending') }}</p>
      <template v-if="balance">
        <p>{{ t('upstreamInfo.balanceAvailability') }}: <strong :class="{ exhausted: !balance.is_available }">{{ t(balance.is_available ? 'upstreamInfo.balanceAvailable' : 'upstreamInfo.balanceInsufficient') }}</strong></p>
        <section v-for="row in balance.balance_infos" :key="row.currency" class="balance">
          <h3>{{ t('upstreamInfo.wallet') }} · {{ row.currency }}</h3>
          <strong class="total">{{ row.total_balance }} {{ row.currency }}</strong>
          <dl><dt>{{ t('upstreamInfo.grantedBalance') }}</dt><dd>{{ row.granted_balance }} {{ row.currency }}</dd><dt>{{ t('upstreamInfo.toppedUpBalance') }}</dt><dd>{{ row.topped_up_balance }} {{ row.currency }}</dd></dl>
        </section>
        <p v-if="!balance.balance_infos.length" class="hint">{{ t('upstreamInfo.noData') }}</p>
      </template>
    </div>
  </BaseModal>
</template>
<style scoped>
.deepseek-detail { color: var(--mac-text); font-size: 13px; }.toolbar { display: flex; justify-content: space-between; align-items: center; gap: 12px; margin-bottom: 12px; }.hint { color: var(--mac-text-secondary); line-height: 1.6; }.balance { border: 1px solid var(--mac-border); border-radius: 12px; padding: 16px; margin-top: 14px; }h3 { font-size: 14px; margin: 0 0 10px; }.total { display: block; font-size: 24px; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }.exhausted { color: #dc5a50; }
dl { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 10px; margin: 14px 0 0; }dt { color: var(--mac-text-secondary); }dd { margin: 0; text-align: right; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }button { border: 0; background: transparent; color: var(--mac-accent, #0a84ff); cursor: pointer; white-space: nowrap; }button:disabled { opacity: .5; cursor: wait; }
@media (max-width: 480px) { .toolbar { align-items: start; }.balance { padding: 12px; } }
</style>
