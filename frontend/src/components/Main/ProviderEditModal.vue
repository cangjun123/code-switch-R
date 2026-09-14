<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Listbox,
  ListboxButton,
  ListboxOptions,
  ListboxOption,
} from '@headlessui/vue'
import BaseModal from '../common/BaseModal.vue'
import BaseButton from '../common/BaseButton.vue'
import BaseInput from '../common/BaseInput.vue'
import ModelWhitelistEditor from '../common/ModelWhitelistEditor.vue'
import ModelMappingEditor from '../common/ModelMappingEditor.vue'
import CLIConfigEditor from '../common/CLIConfigEditor.vue'
import lobeIcons from '../../icons/lobeIconMap'
import type { CLIPlatform } from '../../services/cliConfig'

export type ProviderTab = 'claude' | 'codex' | 'gemini' | 'gpt-image' | 'others'

export interface ProviderFormState {
  name: string
  apiUrl: string
  officialSite: string
  apiKey: string
  apiEndpoint?: string
  upstreamProtocol?: string
  openAIEndpointMode?: string
  codexMultiAgentNamespaceRewrite?: boolean
  bridgeResponsesInstructions?: boolean
  forceResponsesStoreFalse?: boolean
  dropResponsesFieldsText?: string
  dropImageFieldsText?: string
  imageAsyncMode?: boolean
  icon: string
  level?: number
  supportedModels?: Record<string, boolean>
  modelMapping?: Record<string, string>
  cliConfig?: Record<string, any>
  enabled: boolean
  availabilityMonitorEnabled?: boolean
  connectivityAutoBlacklist?: boolean
  neverBlacklist?: boolean
  [key: string]: any
}

export interface OptionItem {
  value: string
  label: string
  desc?: string
}

const props = defineProps<{
  open: boolean
  editingId: number | null
  tabId: ProviderTab
  form: ProviderFormState
  errors: Record<string, string>
  resolvedTheme: 'light' | 'dark'
  upstreamProtocolOptions: OptionItem[]
  openAiEndpointModeOptions: OptionItem[]
  authTypeOptions: OptionItem[]
  selectedAuthType: string
  customAuthHeader: string
  iconSearchQuery: string
  filteredIconOptions: string[]
  getLevelDescription: (level: number) => string
  activeProxyState: boolean
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'submit'): void
  (e: 'submit-and-apply'): void
  (e: 'update:selectedAuthType', value: string): void
  (e: 'update:customAuthHeader', value: string): void
  (e: 'update:iconSearchQuery', value: string): void
}>()

const { t } = useI18n()

type FormTab = 'general' | 'protocol' | 'routing' | 'cli'
const activeFormTab = ref<FormTab>('general')

const iconSvg = (name: string) => {
  if (!name) return ''
  return lobeIcons[name.toLowerCase()] ?? ''
}

const modalTitle = computed(() =>
  props.editingId ? t('components.main.form.editTitle') : t('components.main.form.createTitle')
)
</script>

<template>
  <BaseModal :open="open" :title="modalTitle" @close="emit('close')">
    <!-- Form Tab Navigation -->
    <div class="modal-tabs">
      <button
        type="button"
        :class="['modal-tab-btn', { active: activeFormTab === 'general' }]"
        @click="activeFormTab = 'general'"
      >
        {{ t('components.main.form.tabs.general', '基本配置') }}
      </button>
      <button
        type="button"
        :class="['modal-tab-btn', { active: activeFormTab === 'protocol' }]"
        @click="activeFormTab = 'protocol'"
      >
        {{ t('components.main.form.tabs.protocol', '协议与高级') }}
      </button>
      <button
        type="button"
        :class="['modal-tab-btn', { active: activeFormTab === 'routing' }]"
        @click="activeFormTab = 'routing'"
      >
        {{ t('components.main.form.tabs.routing', '调度与模型') }}
      </button>
      <button
        v-if="tabId !== 'gpt-image'"
        type="button"
        :class="['modal-tab-btn', { active: activeFormTab === 'cli' }]"
        @click="activeFormTab = 'cli'"
      >
        {{ t('components.main.form.tabs.cli', 'CLI 注入') }}
      </button>
    </div>

    <form class="vendor-form" @submit.prevent="emit('submit')">
      <!-- Tab 1: 基本配置 -->
      <div v-show="activeFormTab === 'general'" class="tab-pane">
        <label class="form-field">
          <span class="label-row">
            {{ t('components.main.form.labels.name') }}
            <span v-if="errors.name" class="field-error">{{ errors.name }}</span>
          </span>
          <BaseInput
            v-model="form.name"
            type="text"
            :placeholder="t('components.main.form.placeholders.name')"
            :disabled="!!editingId && tabId === 'gemini'"
            required
          />
          <span v-if="editingId && tabId !== 'gemini'" class="field-hint">
            {{ t('components.main.form.renameHint') }}
          </span>
        </label>

        <label class="form-field">
          <span class="label-row">
            {{ t('components.main.form.labels.apiUrl') }}
            <span v-if="errors.apiUrl" class="field-error">{{ errors.apiUrl }}</span>
          </span>
          <BaseInput
            v-model="form.apiUrl"
            type="text"
            :placeholder="t('components.main.form.placeholders.apiUrl')"
            required
            :class="{ 'has-error': !!errors.apiUrl }"
          />
        </label>

        <label class="form-field">
          <span>{{ t('components.main.form.labels.apiKey') }}</span>
          <BaseInput
            v-model="form.apiKey"
            type="text"
            :placeholder="t('components.main.form.placeholders.apiKey')"
          />
        </label>

        <label class="form-field">
          <span>{{ t('components.main.form.labels.officialSite') }}</span>
          <BaseInput
            v-model="form.officialSite"
            type="text"
            :placeholder="t('components.main.form.placeholders.officialSite')"
          />
        </label>

        <!-- API 端点（可选）-->
        <label class="form-field">
          <span>{{ t('components.main.form.labels.apiEndpoint') }}</span>
          <BaseInput
            v-model="form.apiEndpoint"
            type="text"
            :placeholder="t('components.main.form.placeholders.apiEndpoint')"
          />
          <span class="field-hint">{{ t('components.main.form.hints.apiEndpoint') }}</span>
        </label>

        <!-- 图标选择 -->
        <div class="form-field">
          <span>{{ t('components.main.form.labels.icon') }}</span>
          <Listbox v-model="form.icon" v-slot="{ open: iconOpen }" class="w-full">
            <div class="icon-select">
              <ListboxButton class="icon-select-button">
                <span class="icon-preview" v-html="iconSvg(form.icon)" aria-hidden="true"></span>
                <span class="icon-select-label">{{ form.icon }}</span>
                <svg viewBox="0 0 20 20" aria-hidden="true">
                  <path d="M6 8l4 4 4-4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" fill="none" />
                </svg>
              </ListboxButton>
              <ListboxOptions v-if="iconOpen" class="icon-select-options">
                <div class="icon-search-wrapper">
                  <input
                    :value="iconSearchQuery"
                    type="text"
                    class="icon-search-input"
                    :placeholder="t('components.main.form.placeholders.searchIcon')"
                    @input="emit('update:iconSearchQuery', ($event.target as HTMLInputElement).value)"
                    @click.stop
                    @keydown.stop
                  />
                </div>
                <ListboxOption
                  v-for="iconName in filteredIconOptions"
                  :key="iconName"
                  :value="iconName"
                  v-slot="{ active, selected }"
                >
                  <div :class="['icon-option', { active, selected }]">
                    <span class="icon-preview" v-html="iconSvg(iconName)" aria-hidden="true"></span>
                    <span class="icon-name">{{ iconName }}</span>
                  </div>
                </ListboxOption>
                <div v-if="filteredIconOptions.length === 0" class="icon-no-results">
                  {{ t('components.main.form.noIconResults') }}
                </div>
              </ListboxOptions>
            </div>
          </Listbox>
        </div>

        <div class="form-field switch-field">
          <span>{{ t('components.main.form.labels.enabled') }}</span>
          <div class="switch-inline">
            <label class="mac-switch">
              <input type="checkbox" v-model="form.enabled" />
              <span></span>
            </label>
            <span class="switch-text">
              {{ form.enabled ? t('components.main.form.switch.on') : t('components.main.form.switch.off') }}
            </span>
          </div>
        </div>
      </div>

      <!-- Tab 2: 协议与高级 -->
      <div v-show="activeFormTab === 'protocol'" class="tab-pane">
        <!-- 上游协议类型 -->
        <div class="form-field">
          <span>{{ t('components.main.form.labels.upstreamProtocol') }}</span>
          <Listbox v-model="form.upstreamProtocol" v-slot="{ open: protoOpen }">
            <div class="level-select">
              <ListboxButton class="level-select-button">
                <span class="level-label">
                  {{ upstreamProtocolOptions.find((item) => item.value === form.upstreamProtocol)?.label || form.upstreamProtocol }}
                </span>
                <svg viewBox="0 0 20 20" aria-hidden="true">
                  <path d="M6 8l4 4 4-4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" fill="none" />
                </svg>
              </ListboxButton>
              <ListboxOptions
                v-if="protoOpen"
                :class="['level-select-options', { dark: resolvedTheme === 'dark' }]"
              >
                <ListboxOption
                  v-for="option in upstreamProtocolOptions"
                  :key="option.value"
                  :value="option.value"
                  v-slot="{ active, selected }"
                >
                  <div :class="['level-option', { active, selected }]">
                    <span class="level-name">{{ option.label }}</span>
                    <span class="level-desc">{{ option.desc }}</span>
                  </div>
                </ListboxOption>
              </ListboxOptions>
            </div>
          </Listbox>
          <span class="field-hint">{{ t('components.main.form.hints.upstreamProtocol') }}</span>
        </div>

        <!-- Codex 专有设置 -->
        <template v-if="tabId === 'codex'">
          <div class="form-field">
            <span>{{ t('components.main.form.labels.openAIEndpointMode') }}</span>
            <Listbox v-model="form.openAIEndpointMode" v-slot="{ open: modeOpen }">
              <div class="level-select">
                <ListboxButton class="level-select-button">
                  <span class="level-label">
                    {{ openAiEndpointModeOptions.find((item) => item.value === form.openAIEndpointMode)?.label || form.openAIEndpointMode }}
                  </span>
                  <svg viewBox="0 0 20 20" aria-hidden="true">
                    <path d="M6 8l4 4 4-4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" fill="none" />
                  </svg>
                </ListboxButton>
                <ListboxOptions
                  v-if="modeOpen"
                  :class="['level-select-options', { dark: resolvedTheme === 'dark' }]"
                >
                  <ListboxOption
                    v-for="option in openAiEndpointModeOptions"
                    :key="option.value"
                    :value="option.value"
                    v-slot="{ active, selected }"
                  >
                    <div :class="['level-option', { active, selected }]">
                      <span class="level-name">{{ option.label }}</span>
                      <span class="level-desc">{{ option.desc }}</span>
                    </div>
                  </ListboxOption>
                </ListboxOptions>
              </div>
            </Listbox>
            <span class="field-hint">{{ t('components.main.form.hints.openAIEndpointMode') }}</span>
          </div>

          <div class="form-field switch-field">
            <span>{{ t('components.main.form.labels.codexMultiAgentNamespaceRewrite') }}</span>
            <div class="switch-inline">
              <label class="mac-switch">
                <input type="checkbox" v-model="form.codexMultiAgentNamespaceRewrite" />
                <span></span>
              </label>
              <span class="switch-text">
                {{ form.codexMultiAgentNamespaceRewrite ? t('components.main.form.switch.on') : t('components.main.form.switch.off') }}
              </span>
            </div>
            <span class="field-hint">{{ t('components.main.form.hints.codexMultiAgentNamespaceRewrite') }}</span>
          </div>

          <div class="form-field switch-field">
            <span>{{ t('components.main.form.labels.bridgeResponsesInstructions') }}</span>
            <div class="switch-inline">
              <label class="mac-switch">
                <input type="checkbox" v-model="form.bridgeResponsesInstructions" />
                <span></span>
              </label>
              <span class="switch-text">
                {{ form.bridgeResponsesInstructions ? t('components.main.form.switch.on') : t('components.main.form.switch.off') }}
              </span>
            </div>
            <span class="field-hint">{{ t('components.main.form.hints.bridgeResponsesInstructions') }}</span>
          </div>

          <div class="form-field switch-field">
            <span>{{ t('components.main.form.labels.forceResponsesStoreFalse') }}</span>
            <div class="switch-inline">
              <label class="mac-switch">
                <input type="checkbox" v-model="form.forceResponsesStoreFalse" />
                <span></span>
              </label>
              <span class="switch-text">
                {{ form.forceResponsesStoreFalse ? t('components.main.form.switch.on') : t('components.main.form.switch.off') }}
              </span>
            </div>
            <span class="field-hint">{{ t('components.main.form.hints.forceResponsesStoreFalse') }}</span>
          </div>

          <label class="form-field">
            <span>{{ t('components.main.form.labels.dropResponsesFields') }}</span>
            <BaseInput
              v-model="form.dropResponsesFieldsText"
              type="text"
              :placeholder="t('components.main.form.placeholders.dropResponsesFields')"
            />
            <span class="field-hint">{{ t('components.main.form.hints.dropResponsesFields') }}</span>
          </label>
        </template>

        <!-- GPT-Image 专有设置 -->
        <template v-if="tabId === 'gpt-image'">
          <label class="form-field">
            <span>{{ t('components.main.form.labels.dropImageFields') }}</span>
            <BaseInput
              v-model="form.dropImageFieldsText"
              type="text"
              :placeholder="t('components.main.form.placeholders.dropImageFields')"
            />
            <span class="field-hint">{{ t('components.main.form.hints.dropImageFields') }}</span>
          </label>

          <div class="form-field switch-field">
            <span>{{ t('components.main.form.labels.imageAsyncMode') }}</span>
            <div class="switch-inline">
              <label class="mac-switch">
                <input type="checkbox" v-model="form.imageAsyncMode" />
                <span></span>
              </label>
              <span class="switch-text">
                {{ form.imageAsyncMode ? t('components.main.form.switch.on') : t('components.main.form.switch.off') }}
              </span>
            </div>
            <span class="field-hint">{{ t('components.main.form.hints.imageAsyncMode') }}</span>
          </div>
        </template>

        <!-- 认证方式 -->
        <div class="form-field">
          <span>{{ t('components.main.form.labels.connectivityAuthType') }}</span>
          <Listbox
            :model-value="selectedAuthType"
            @update:model-value="emit('update:selectedAuthType', $event)"
            v-slot="{ open: authOpen }"
          >
            <div class="level-select">
              <ListboxButton class="level-select-button">
                <span class="level-label">
                  {{ authTypeOptions.find((item) => item.value === selectedAuthType)?.label || selectedAuthType }}
                </span>
                <svg viewBox="0 0 20 20" aria-hidden="true">
                  <path d="M6 8l4 4 4-4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" fill="none" />
                </svg>
              </ListboxButton>
              <ListboxOptions
                v-if="authOpen"
                :class="['level-select-options', { dark: resolvedTheme === 'dark' }]"
              >
                <ListboxOption
                  v-for="option in authTypeOptions"
                  :key="option.value"
                  :value="option.value"
                  v-slot="{ active, selected }"
                >
                  <div :class="['level-option', { active, selected }]">
                    <span class="level-name">{{ option.label }}</span>
                  </div>
                </ListboxOption>
              </ListboxOptions>
            </div>
          </Listbox>
          <BaseInput
            :model-value="customAuthHeader"
            @update:model-value="emit('update:customAuthHeader', $event)"
            type="text"
            :placeholder="t('components.main.form.placeholders.customAuthHeader')"
            class="mt-2"
          />
          <span class="field-hint">{{ t('components.main.form.hints.connectivityAuthType') }}</span>
        </div>
      </div>

      <!-- Tab 3: 调度与模型 -->
      <div v-show="activeFormTab === 'routing'" class="tab-pane">
        <!-- 优先级权重 Level -->
        <div class="form-field">
          <span>{{ t('components.main.form.labels.level') }}</span>
          <Listbox v-model="form.level" v-slot="{ open: levelOpen }">
            <div class="level-select">
              <ListboxButton class="level-select-button">
                <span class="level-badge" :class="`level-${form.level || 1}`">
                  L{{ form.level || 1 }}
                </span>
                <span class="level-label">
                  Level {{ form.level || 1 }} - {{ getLevelDescription(form.level || 1) }}
                </span>
                <svg viewBox="0 0 20 20" aria-hidden="true">
                  <path d="M6 8l4 4 4-4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" fill="none" />
                </svg>
              </ListboxButton>
              <ListboxOptions
                v-if="levelOpen"
                :class="['level-select-options', { dark: resolvedTheme === 'dark' }]"
              >
                <ListboxOption
                  v-for="lvl in 10"
                  :key="lvl"
                  :value="lvl"
                  v-slot="{ active, selected }"
                >
                  <div :class="['level-option', { active, selected }]">
                    <span class="level-badge" :class="`level-${lvl}`">L{{ lvl }}</span>
                    <span class="level-name">Level {{ lvl }} - {{ getLevelDescription(lvl) }}</span>
                  </div>
                </ListboxOption>
              </ListboxOptions>
            </div>
          </Listbox>
          <span class="field-hint">{{ t('components.main.form.hints.level') }}</span>
        </div>

        <!-- 模型白名单与映射 -->
        <div class="form-field">
          <ModelWhitelistEditor v-model="form.supportedModels" />
        </div>

        <div class="form-field">
          <ModelMappingEditor v-model="form.modelMapping" />
        </div>

        <!-- 可用性监控 -->
        <div class="form-field switch-field">
          <span>{{ t('components.main.form.labels.availabilityMonitor') }}</span>
          <div class="switch-inline">
            <label class="mac-switch">
              <input type="checkbox" v-model="form.availabilityMonitorEnabled" />
              <span></span>
            </label>
            <span class="switch-text">
              {{ form.availabilityMonitorEnabled ? t('components.main.form.switch.on') : t('components.main.form.switch.off') }}
            </span>
          </div>
          <span class="field-hint">{{ t('components.main.form.hints.availabilityMonitor') }}</span>
        </div>

        <!-- 连通性自动拉黑 -->
        <div v-if="form.availabilityMonitorEnabled" class="form-field switch-field">
          <span>{{ t('components.main.form.labels.connectivityAutoBlacklist') }}</span>
          <div class="switch-inline">
            <label class="mac-switch">
              <input type="checkbox" v-model="form.connectivityAutoBlacklist" />
              <span></span>
            </label>
            <span class="switch-text">
              {{ form.connectivityAutoBlacklist ? t('components.main.form.switch.on') : t('components.main.form.switch.off') }}
            </span>
          </div>
          <span class="field-hint">{{ t('components.main.form.hints.connectivityAutoBlacklist') }}</span>
        </div>

        <!-- 永不拉黑 -->
        <div v-if="tabId !== 'others' && tabId !== 'gemini'" class="form-field switch-field">
          <span>{{ t('components.main.form.labels.neverBlacklist') }}</span>
          <div class="switch-inline">
            <label class="mac-switch">
              <input type="checkbox" v-model="form.neverBlacklist" />
              <span></span>
            </label>
            <span class="switch-text">
              {{ form.neverBlacklist ? t('components.main.form.switch.on') : t('components.main.form.switch.off') }}
            </span>
          </div>
          <span class="field-hint">{{ t('components.main.form.hints.neverBlacklist') }}</span>
        </div>
      </div>

      <!-- Tab 4: CLI 注入 -->
      <div v-show="activeFormTab === 'cli'" class="tab-pane">
        <template v-if="tabId !== 'gpt-image'">
          <CLIConfigEditor
            :platform="tabId as CLIPlatform"
            v-model="form.cliConfig"
            :provider-config="{
              apiKey: form.apiKey,
              baseUrl: form.apiUrl
            }"
          />
        </template>
      </div>

      <!-- 底部动作条 -->
      <footer class="form-actions">
        <BaseButton variant="outline" type="button" @click="emit('close')">
          {{ t('components.main.form.actions.cancel') }}
        </BaseButton>
        <BaseButton type="submit">
          {{ t('components.main.form.actions.save') }}
        </BaseButton>
        <!-- 保存并应用：仅在编辑模式、非代理模式、非 others/gpt-image 平台时显示 -->
        <BaseButton
          v-if="editingId && tabId !== 'others' && tabId !== 'gpt-image' && !activeProxyState"
          type="button"
          variant="primary"
          @click="emit('submit-and-apply')"
        >
          {{ t('components.main.form.actions.saveAndApply') }}
        </BaseButton>
      </footer>
    </form>
  </BaseModal>
</template>

<style scoped>
.modal-tabs {
  display: flex;
  gap: 6px;
  border-bottom: 1px solid var(--mac-border);
  padding-bottom: 10px;
  margin-bottom: 16px;
  overflow-x: auto;
}

.modal-tab-btn {
  border: none;
  background: transparent;
  color: var(--mac-text-secondary);
  font-size: 0.88rem;
  font-weight: 500;
  padding: 6px 14px;
  border-radius: 8px;
  cursor: pointer;
  transition: all 0.15s ease;
  white-space: nowrap;
}

.modal-tab-btn:hover {
  background: rgba(15, 23, 42, 0.05);
  color: var(--mac-text);
}

:global(.dark) .modal-tab-btn:hover {
  background: rgba(255, 255, 255, 0.08);
}

.modal-tab-btn.active {
  background: var(--mac-accent);
  color: #fff;
  font-weight: 600;
}

.tab-pane {
  display: flex;
  flex-direction: column;
  gap: 16px;
  min-height: 280px;
}
</style>
