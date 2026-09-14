<template>
  <BaseModal :open="open" :title="t('components.main.modelTrace.title')" @close="$emit('close')">
    <div class="modeltrace-body">
      <!-- 第一步：选择要检测的模型 -->
      <div class="modeltrace-section">
        <p class="modeltrace-label">
          {{ t('components.main.modelTrace.selectModel') }}
          <span class="modeltrace-provider">· {{ providerName }}</span>
        </p>
        <div class="modeltrace-model-grid">
          <button
            v-for="model in models"
            :key="model.id"
            type="button"
            :class="['modeltrace-model-chip', `family-${model.family}`, { selected: selectedModel === model.id }]"
            @click="selectedModel = model.id"
          >
            {{ model.displayName || model.id }}
          </button>
        </div>
      </div>

      <!-- 操作按钮 -->
      <div class="modeltrace-actions">
        <BaseButton
          type="button"
          :disabled="!selectedModel || verifying"
          @click="handleVerify"
        >
          <span v-if="verifying" class="btn-spinner"></span>
          {{ verifying ? t('components.main.modelTrace.verifying') : t('components.main.modelTrace.verify') }}
        </BaseButton>
      </div>

      <!-- 检测结果 -->
      <div v-if="result" class="modeltrace-result">
        <!-- 判定横幅 -->
        <div v-if="result.verdict === 'match'" class="modeltrace-verdict verdict-match">
          <span class="verdict-icon">✓</span>
          <div class="verdict-text">
            <p class="verdict-title">{{ t('components.main.modelTrace.match') }}</p>
            <p class="verdict-detail">
              {{ t('components.main.modelTrace.matchDetail', {
                model: result.topModelName || result.topModel,
                probability: formatProbability(result.topProbability)
              }) }}
            </p>
          </div>
        </div>
        <div v-else-if="result.verdict === 'mismatch'" class="modeltrace-verdict verdict-mismatch">
          <span class="verdict-icon">⚠</span>
          <div class="verdict-text">
            <p class="verdict-title">{{ t('components.main.modelTrace.mismatch') }}</p>
            <p class="verdict-detail">
              {{ t('components.main.modelTrace.mismatchDetail', {
                expected: result.expectedModel,
                actual: result.topModelName || result.topModel,
                probability: formatProbability(result.topProbability)
              }) }}
            </p>
          </div>
        </div>
        <div v-else class="modeltrace-verdict verdict-error">
          <span class="verdict-icon">✕</span>
          <div class="verdict-text">
            <p class="verdict-title">{{ t('components.main.modelTrace.error') }}</p>
            <p class="verdict-detail">{{ result.message }}</p>
          </div>
        </div>

        <!-- 概率分布（仅成功时展示 top 6） -->
        <template v-if="result.success && result.probabilities?.length">
          <p class="modeltrace-label">{{ t('components.main.modelTrace.distributionTitle') }}</p>
          <div class="modeltrace-bars">
            <div
              v-for="item in topProbabilities"
              :key="item.model"
              :class="['modeltrace-bar-row', {
                'is-expected': item.model === result.expectedModel,
                'is-top': item.model === result.topModel
              }]"
            >
              <span class="bar-model">{{ item.displayName || item.model }}</span>
              <div class="bar-track">
                <div
                  class="bar-fill"
                  :class="item.model === result.topModel ? 'fill-top' : 'fill-normal'"
                  :style="{ width: barWidth(item.probability) }"
                ></div>
              </div>
              <span class="bar-value">{{ formatProbability(item.probability) }}</span>
            </div>
          </div>
          <div class="modeltrace-meta">
            <span>{{ t('components.main.modelTrace.validNumbers') }}: {{ result.validNumberCount }}</span>
            <span>{{ t('components.main.modelTrace.attempts') }}: {{ result.attempts }}</span>
            <span>{{ t('components.main.modelTrace.latency') }}: {{ result.latencyMs }}ms</span>
          </div>
        </template>
      </div>
    </div>

    <footer class="form-actions">
      <BaseButton
        v-if="result?.success"
        variant="outline"
        type="button"
        :disabled="verifying"
        @click="handleVerify"
      >
        {{ t('components.main.modelTrace.retry') }}
      </BaseButton>
      <BaseButton variant="outline" type="button" @click="$emit('close')">
        {{ t('components.main.modelTrace.close') }}
      </BaseButton>
    </footer>
  </BaseModal>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseButton from '../common/BaseButton.vue'
import BaseModal from '../common/BaseModal.vue'
import {
  getSupportedModels,
  verifyProviderModel,
  type ModelTraceModelOption,
  type ModelTraceResult,
} from '../../services/modeltrace'
import { extractErrorMessage } from '../../utils/error'

const props = defineProps<{
  open: boolean
  platform: string
  providerId: number
  providerName: string
}>()

defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()

const models = ref<ModelTraceModelOption[]>([])
const selectedModel = ref('')
const verifying = ref(false)
const result = ref<ModelTraceResult | null>(null)

// 打开时加载指纹库模型列表
watch(
  () => props.open,
  async (open) => {
    if (!open) return
    result.value = null
    verifying.value = false
    if (models.value.length === 0) {
      try {
        models.value = await getSupportedModels()
      } catch (error) {
        console.error('Failed to load supported models', error)
      }
    }
  },
)

const topProbabilities = computed(() =>
  (result.value?.probabilities ?? []).slice(0, 6),
)

const formatProbability = (value: number) => `${(value * 100).toFixed(1)}%`

const barWidth = (probability: number) => {
  const max = result.value?.topProbability || 1
  const ratio = max > 0 ? probability / max : 0
  return `${Math.max(ratio * 100, probability > 0 ? 2 : 0)}%`
}

const handleVerify = async () => {
  if (!selectedModel.value || verifying.value) return
  verifying.value = true
  result.value = null
  try {
    result.value = await verifyProviderModel(
      props.platform,
      props.providerId,
      selectedModel.value,
    )
  } catch (error) {
    result.value = {
      success: false,
      message: extractErrorMessage(error),
      expectedModel: selectedModel.value,
      expectedMatched: false,
      topModel: '',
      topModelName: '',
      topProbability: 0,
      verdict: 'error',
      validNumberCount: 0,
      attempts: 0,
      latencyMs: 0,
      probabilities: [],
    }
  } finally {
    verifying.value = false
  }
}
</script>

<style scoped>
.modeltrace-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.modeltrace-label {
  font-size: 13px;
  font-weight: 600;
  color: var(--mac-text, #1f2937);
  margin: 0 0 8px;
}

.modeltrace-provider {
  font-weight: 400;
  opacity: 0.7;
}

.modeltrace-model-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.modeltrace-model-chip {
  padding: 6px 12px;
  border-radius: 999px;
  border: 1px solid rgba(15, 23, 42, 0.18);
  background: rgba(15, 23, 42, 0.04);
  color: var(--mac-text, #1f2937);
  font-size: 12.5px;
  cursor: pointer;
  transition: all 0.15s ease;
}

.modeltrace-model-chip:hover {
  border-color: rgba(15, 23, 42, 0.4);
}

.modeltrace-model-chip.selected {
  background: rgba(10, 132, 255, 0.14);
  border-color: #0a84ff;
  color: #0a84ff;
  font-weight: 600;
}

.modeltrace-model-chip.family-claude.selected {
  background: rgba(217, 119, 87, 0.14);
  border-color: #d97757;
  color: #d97757;
}

:global(html.dark) .modeltrace-model-chip {
  border-color: rgba(255, 255, 255, 0.2);
  background: rgba(255, 255, 255, 0.06);
}

.modeltrace-actions {
  display: flex;
  justify-content: flex-end;
}

.modeltrace-result {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.modeltrace-verdict {
  display: flex;
  gap: 12px;
  align-items: flex-start;
  padding: 12px 14px;
  border-radius: 10px;
}

.verdict-match {
  background: rgba(34, 197, 94, 0.1);
  border-left: 3px solid #22c55e;
}

.verdict-mismatch {
  background: rgba(239, 68, 68, 0.1);
  border-left: 3px solid #ef4444;
}

.verdict-error {
  background: rgba(239, 68, 68, 0.08);
  border-left: 3px solid #f59e0b;
}

.verdict-icon {
  font-size: 18px;
  line-height: 1.2;
}

.verdict-match .verdict-icon { color: #16a34a; }
.verdict-mismatch .verdict-icon { color: #dc2626; }
.verdict-error .verdict-icon { color: #d97706; }

.verdict-text { flex: 1; }

.verdict-title {
  margin: 0 0 2px;
  font-size: 14px;
  font-weight: 700;
}

.verdict-match .verdict-title { color: #16a34a; }
.verdict-mismatch .verdict-title { color: #dc2626; }
.verdict-error .verdict-title { color: #b45309; }

.verdict-detail {
  margin: 0;
  font-size: 12.5px;
  line-height: 1.5;
  color: var(--mac-text, #374151);
  opacity: 0.85;
  word-break: break-word;
}

.modeltrace-bars {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.modeltrace-bar-row {
  display: grid;
  grid-template-columns: 150px 1fr 52px;
  align-items: center;
  gap: 10px;
  font-size: 12px;
  padding: 2px 4px;
  border-radius: 6px;
}

.modeltrace-bar-row.is-top {
  background: rgba(10, 132, 255, 0.07);
}

.modeltrace-bar-row.is-expected .bar-model::after {
  content: ' ✓';
  color: #16a34a;
  font-weight: 700;
}

.bar-model {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.bar-track {
  height: 10px;
  border-radius: 5px;
  background: rgba(15, 23, 42, 0.08);
  overflow: hidden;
}

:global(html.dark) .bar-track {
  background: rgba(255, 255, 255, 0.12);
}

.bar-fill {
  height: 100%;
  border-radius: 5px;
  transition: width 0.4s ease;
}

.fill-top { background: #0a84ff; }
.fill-normal { background: rgba(10, 132, 255, 0.35); }

.bar-value {
  text-align: right;
  font-variant-numeric: tabular-nums;
  opacity: 0.75;
}

.modeltrace-meta {
  display: flex;
  gap: 14px;
  flex-wrap: wrap;
  font-size: 11.5px;
  opacity: 0.65;
}
</style>
