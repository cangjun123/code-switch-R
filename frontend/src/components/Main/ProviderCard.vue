<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AutomationCard } from '../../data/cards'
import lobeIcons from '../../icons/lobeIconMap'

export type ProviderTab = 'claude' | 'codex' | 'gpt-image' | 'gemini' | 'others'

export interface BlacklistStatus {
  isBlacklisted: boolean
  blacklistLevel: number
  remainingSeconds: number
}

export interface ProviderStatDisplay {
  state: 'loading' | 'ready' | 'empty'
  message?: string
  successRateLabel?: string
  successRateClass?: string
  requests?: string
  tokens?: string
  cost?: string
}

const props = defineProps<{
  card: AutomationCard
  activeTab: ProviderTab
  activeProxyState: boolean
  isLastUsed: boolean
  isHighlighted: boolean
  isDirectApplied: boolean
  isDragging: boolean
  blacklistStatus: BlacklistStatus | null
  stats: ProviderStatDisplay
  resolvedTheme: 'light' | 'dark'
  connectivityIndicatorClass: string
  connectivityTooltip: string
  showModelTraceButton: boolean
}>()

const emit = defineEmits<{
  (e: 'dragstart', id: number, event: DragEvent): void
  (e: 'dragend'): void
  (e: 'drop', id: number): void
  (e: 'toggle-enabled', card: AutomationCard): void
  (e: 'direct-apply', card: AutomationCard): void
  (e: 'open-model-trace', card: AutomationCard): void
  (e: 'configure', card: AutomationCard): void
  (e: 'duplicate', card: AutomationCard): void
  (e: 'remove', card: AutomationCard): void
  (e: 'unblock', name: string): void
  (e: 'reset-level', name: string): void
  (e: 'open-official-site', url: string): void
}>()

const { t } = useI18n()

const iconSvg = (name: string) => {
  if (!name) return ''
  return lobeIcons[name.toLowerCase()] ?? ''
}

const vendorInitials = (name: string) => {
  if (!name) return 'AI'
  return name
    .split(/\s+/)
    .filter(Boolean)
    .map((word) => word[0])
    .join('')
    .slice(0, 2)
    .toUpperCase()
}

const formatOfficialSite = (site: string) => {
  if (!site) return ''
  try {
    const target = site.startsWith('http://') || site.startsWith('https://') ? site : `https://${site}`
    const url = new URL(target)
    return url.hostname.replace(/^www\./, '')
  } catch {
    return site
  }
}

const formatBlacklistCountdown = (remainingSeconds: number): string => {
  const minutes = Math.floor(remainingSeconds / 60)
  const seconds = remainingSeconds % 60
  return `${minutes}${t('components.main.blacklist.minutes')}${seconds}${t('components.main.blacklist.seconds')}`
}

const showBlBadge = computed(() => {
  return props.blacklistStatus && props.blacklistStatus.blacklistLevel > 0
})
</script>

<template>
  <article
    :class="[
      'automation-card',
      { dragging: isDragging },
      { 'is-last-used': isLastUsed },
      { 'is-highlighted': isHighlighted }
    ]"
    draggable="true"
    @dragstart="emit('dragstart', card.id, $event)"
    @dragend="emit('dragend')"
    @drop="emit('drop', card.id)"
  >
    <!-- 正在使用标签 -->
    <span v-if="isLastUsed" class="last-used-badge">
      ✓ {{ t('components.main.providers.lastUsed') }}
    </span>

    <div class="card-leading">
      <div class="card-icon" :style="{ backgroundColor: card.tint, color: card.accent }">
        <span v-if="!iconSvg(card.icon)" class="icon-fallback">
          {{ vendorInitials(card.name) }}
        </span>
        <span v-else class="icon-svg" v-html="iconSvg(card.icon)" aria-hidden="true"></span>
      </div>

      <div class="card-text">
        <div class="card-title-row">
          <p class="card-title">{{ card.name }}</p>

          <!-- 直连正在使用徽章 -->
          <span
            v-if="isDirectApplied && !activeProxyState"
            class="current-use-badge"
          >
            {{ t('components.main.directApply.currentBadge') }}
          </span>

          <!-- 连通性指示器 -->
          <span
            v-if="card.availabilityMonitorEnabled"
            class="connectivity-dot"
            :class="connectivityIndicatorClass"
            :title="connectivityTooltip"
          ></span>

          <!-- 调度等级徽章 -->
          <span v-if="card.level" class="level-badge scheduling-level" :class="`level-${card.level}`">
            L{{ card.level }}
          </span>

          <!-- 黑名单等级徽章：仅在等级大于0时显示，避免BL0视觉噪音 -->
          <span
            v-if="showBlBadge"
            :class="[
              'blacklist-level-badge',
              `bl-level-${blacklistStatus!.blacklistLevel}`,
              { dark: resolvedTheme === 'dark' }
            ]"
            :title="t('components.main.blacklist.levelTitle', { level: blacklistStatus!.blacklistLevel })"
          >
            BL{{ blacklistStatus!.blacklistLevel }}
          </span>

          <!-- 永不拉黑徽章 -->
          <span
            v-if="card.neverBlacklist"
            class="never-blacklist-badge"
            :class="{ dark: resolvedTheme === 'dark' }"
            :title="t('components.main.blacklist.neverBlacklistHint')"
          >
            🛡️
          </span>

          <!-- 官网链接 -->
          <button
            v-if="card.officialSite"
            class="card-site"
            type="button"
            @click.stop="emit('open-official-site', card.officialSite)"
          >
            {{ formatOfficialSite(card.officialSite) }}
          </button>
        </div>

        <!-- 卡片运行指标 -->
        <p class="card-metrics">
          <template v-if="stats.state !== 'ready'">
            {{ stats.message }}
          </template>
          <template v-else>
            <span
              v-if="stats.successRateLabel"
              class="card-success-rate"
              :class="stats.successRateClass"
            >
              {{ stats.successRateLabel }}
            </span>
            <span class="card-metric-separator" aria-hidden="true">·</span>
            <span>{{ stats.requests }}</span>
            <span class="card-metric-separator" aria-hidden="true">·</span>
            <span>{{ stats.tokens }}</span>
            <span class="card-metric-separator" aria-hidden="true">·</span>
            <span>{{ stats.cost }}</span>
          </template>
        </p>

        <!-- 黑名单横幅 -->
        <div
          v-if="blacklistStatus?.isBlacklisted"
          :class="['blacklist-banner', { dark: resolvedTheme === 'dark' }]"
        >
          <div class="blacklist-info">
            <span class="blacklist-icon">⛔</span>
            <span
              v-if="blacklistStatus.blacklistLevel > 0"
              :class="['level-badge', `level-${blacklistStatus.blacklistLevel}`, { dark: resolvedTheme === 'dark' }]"
            >
              L{{ blacklistStatus.blacklistLevel }}
            </span>
            <span class="blacklist-text">
              {{ t('components.main.blacklist.blocked') }} |
              {{ t('components.main.blacklist.remaining') }}:
              {{ formatBlacklistCountdown(blacklistStatus.remainingSeconds) }}
            </span>
          </div>
          <div class="blacklist-actions">
            <button
              class="unblock-btn primary"
              type="button"
              @click.stop="emit('unblock', card.name)"
              :title="t('components.main.blacklist.unblockAndResetHint')"
            >
              {{ t('components.main.blacklist.unblockAndReset') }}
            </button>
            <button
              class="unblock-btn secondary"
              type="button"
              @click.stop="emit('reset-level', card.name)"
              :title="t('components.main.blacklist.resetLevelHint')"
            >
              {{ t('components.main.blacklist.resetLevel') }}
            </button>
          </div>
        </div>

        <!-- 等级徽章（未拉黑但有降级） -->
        <div
          v-else-if="blacklistStatus && blacklistStatus.blacklistLevel > 0"
          class="level-badge-standalone"
        >
          <span
            :class="['level-badge', `level-${blacklistStatus.blacklistLevel}`, { dark: resolvedTheme === 'dark' }]"
          >
            L{{ blacklistStatus.blacklistLevel }}
          </span>
          <span class="level-hint">{{ t('components.main.blacklist.levelHint') }}</span>
          <button
            class="reset-level-mini"
            type="button"
            @click.stop="emit('reset-level', card.name)"
            :title="t('components.main.blacklist.resetLevelHint')"
          >
            ✕
          </button>
        </div>
      </div>
    </div>

    <!-- 卡片操作区 -->
    <div class="card-actions">
      <!-- 主启用开关 -->
      <label class="mac-switch sm" :title="t('components.main.form.labels.enabled')">
        <input type="checkbox" v-model="card.enabled" @change="emit('toggle-enabled', card)" />
        <span></span>
      </label>

      <!-- 直连应用按钮 -->
      <button
        v-if="activeTab !== 'others' && activeTab !== 'gpt-image'"
        class="ghost-icon direct-apply-btn"
        :class="{ 'is-active': isDirectApplied && !activeProxyState }"
        :disabled="activeProxyState"
        :data-tooltip="activeProxyState ? t('components.main.directApply.proxyEnabled') : (isDirectApplied ? t('components.main.directApply.inUse') : t('components.main.directApply.title'))"
        @click.stop="!isDirectApplied && emit('direct-apply', card)"
      >
        <span v-if="isDirectApplied && !activeProxyState" class="apply-text">{{ t('components.main.directApply.inUse') }}</span>
        <svg v-else viewBox="0 0 24 24" aria-hidden="true" class="lightning-icon">
          <path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z" stroke="currentColor" stroke-width="1.5" fill="none" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      </button>

      <!-- 编辑配置按钮 -->
      <button
        class="ghost-icon"
        :data-tooltip="t('components.main.form.editTitle')"
        @click="emit('configure', card)"
      >
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path
            d="M11.983 2.25a1.125 1.125 0 011.077.81l.563 2.101a7.482 7.482 0 012.326 1.343l2.08-.621a1.125 1.125 0 011.356.651l1.313 3.207a1.125 1.125 0 01-.442 1.339l-1.86 1.205a7.418 7.418 0 010 2.686l1.86 1.205a1.125 1.125 0 01.442 1.339l-1.313 3.207a1.125 1.125 0 01-1.356.651l-2.08-.621a7.482 7.482 0 01-2.326 1.343l-.563 2.101a1.125 1.125 0 01-1.077.81h-2.634a1.125 1.125 0 01-1.077-.81l-.563-2.101a7.482 7.482 0 01-2.326-1.343l-2.08.621a1.125 1.125 0 01-1.356-.651l-1.313-3.207a1.125 1.125 0 01.442-1.339l1.86-1.205a7.418 7.418 0 010-2.686l-1.86-1.205a1.125 1.125 0 01-.442-1.339l1.313-3.207a1.125 1.125 0 011.356-.651l2.08.621a7.482 7.482 0 012.326-1.343l.563-2.101a1.125 1.125 0 011.077-.81h2.634z"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
          <path d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" />
        </svg>
      </button>

      <!-- 模型真伪检测按钮 -->
      <button
        v-if="showModelTraceButton"
        class="ghost-icon"
        :data-tooltip="t('components.main.modelTrace.tooltip')"
        @click.stop="emit('open-model-trace', card)"
      >
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path
            d="M12 3l7 3v5c0 4.6-3 8.4-7 9.5C8 19.4 5 15.6 5 11V6l7-3z"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
          <path
            d="M9 11.5l2 2 4-4"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </button>

      <!-- 复制按钮 -->
      <button
        class="ghost-icon"
        :data-tooltip="t('components.main.controls.duplicate')"
        @click="emit('duplicate', card)"
      >
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path
            d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </button>

      <!-- 删除按钮 -->
      <button
        class="ghost-icon danger-icon"
        :data-tooltip="t('components.main.form.actions.delete')"
        @click="emit('remove', card)"
      >
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <path
            d="M9 3h6m-7 4h8m-6 0v11m4-11v11M5 7h14l-.867 12.138A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.862L5 7z"
            fill="none"
            stroke="currentColor"
            stroke-width="1.5"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </button>
    </div>
  </article>
</template>

<style scoped>
.automation-card.is-last-used {
  position: relative;
  border: 2px solid rgb(16, 185, 129);
  box-shadow: 0 0 8px rgba(16, 185, 129, 0.3);
}

.last-used-badge {
  position: absolute;
  top: -10px;
  right: 12px;
  background: rgb(16, 185, 129);
  color: white;
  font-size: 10px;
  font-weight: 600;
  padding: 2px 8px;
  border-radius: 4px;
  z-index: 1;
}

.automation-card.is-highlighted {
  animation: highlight-pulse 0.6s ease-in-out 3;
  border-color: rgb(245, 158, 11);
  box-shadow: 0 0 12px rgba(245, 158, 11, 0.5);
}

@keyframes highlight-pulse {
  0%, 100% {
    box-shadow: 0 0 8px rgba(245, 158, 11, 0.3);
  }
  50% {
    box-shadow: 0 0 20px rgba(245, 158, 11, 0.7);
  }
}

:global(.dark) .automation-card.is-last-used {
  border-color: rgb(52, 211, 153);
  box-shadow: 0 0 8px rgba(52, 211, 153, 0.3);
}

:global(.dark) .last-used-badge {
  background: rgb(52, 211, 153);
  color: rgb(6, 78, 59);
}

:global(.dark) .automation-card.is-highlighted {
  border-color: rgb(251, 191, 36);
  box-shadow: 0 0 12px rgba(251, 191, 36, 0.5);
}

.level-badge {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 32px;
  height: 22px;
  padding: 0 7px;
  border-radius: 8px;
  font-size: 11px;
  font-weight: 600;
  line-height: 1;
  letter-spacing: 0.03em;
  text-align: center;
  transition: all 0.2s ease;
}

.card-title-row .level-badge {
  margin-left: 8px;
}

.card-title-row .blacklist-level-badge {
  margin-left: 4px;
}

.never-blacklist-badge {
  margin-left: 4px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 22px;
  height: 22px;
  border-radius: 8px;
  font-size: 12px;
  line-height: 1;
  background: rgba(59, 130, 246, 0.12);
}

.never-blacklist-badge.dark {
  background: rgba(59, 130, 246, 0.22);
}

.level-badge.level-1 {
  background: rgba(16, 185, 129, 0.12);
  color: rgb(5, 150, 105);
}

.level-badge.level-2 {
  background: rgba(34, 197, 94, 0.12);
  color: rgb(22, 163, 74);
}

.level-badge.level-3 {
  background: rgba(132, 204, 22, 0.12);
  color: rgb(101, 163, 13);
}

.level-badge.level-4 {
  background: rgba(234, 179, 8, 0.12);
  color: rgb(161, 98, 7);
}

.level-badge.level-5 {
  background: rgba(245, 158, 11, 0.12);
  color: rgb(180, 83, 9);
}

.level-badge.level-6 {
  background: rgba(249, 115, 22, 0.12);
  color: rgb(194, 65, 12);
}

.level-badge.level-7 {
  background: rgba(239, 68, 68, 0.12);
  color: rgb(185, 28, 28);
}

.level-badge.level-8 {
  background: rgba(220, 38, 38, 0.12);
  color: rgb(153, 27, 27);
}

.level-badge.level-9 {
  background: rgba(190, 18, 60, 0.12);
  color: rgb(159, 18, 57);
}

.level-badge.level-10 {
  background: rgba(159, 18, 57, 0.12);
  color: rgb(136, 19, 55);
}

.blacklist-level-badge {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 32px;
  height: 22px;
  padding: 0 7px;
  border-radius: 8px;
  font-size: 11px;
  font-weight: 600;
  line-height: 1;
  letter-spacing: 0.03em;
  text-align: center;
  transition: all 0.2s ease;
}

.blacklist-level-badge.bl-level-0 {
  background: rgba(107, 114, 128, 0.12);
  color: rgb(75, 85, 99);
}

.blacklist-level-badge.bl-level-1 {
  background: rgba(239, 68, 68, 0.12);
  color: rgb(220, 38, 38);
}

.blacklist-level-badge.bl-level-2 {
  background: rgba(220, 38, 38, 0.16);
  color: rgb(185, 28, 28);
}

.blacklist-level-badge.bl-level-3 {
  background: rgba(185, 28, 28, 0.2);
  color: rgb(153, 27, 27);
}

.blacklist-level-badge.bl-level-4 {
  background: rgba(153, 27, 27, 0.24);
  color: rgb(127, 29, 29);
}

.blacklist-level-badge.bl-level-5 {
  background: rgba(0, 0, 0, 0.15);
  color: rgb(0, 0, 0);
  border: 1px solid rgba(0, 0, 0, 0.2);
}

.card-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

.danger-icon:hover {
  color: #ef4444 !important;
  background: rgba(239, 68, 68, 0.1) !important;
}

/* 黑名单横幅 (醒目的红色背景与警示边框) */
.blacklist-banner {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 10px 12px;
  margin-top: 10px;
  background: rgba(239, 68, 68, 0.12);
  border: 1px solid rgba(239, 68, 68, 0.3);
  border-left: 4px solid #ef4444;
  border-radius: 8px;
  font-size: 13px;
  color: #dc2626;
  box-shadow: 0 2px 8px rgba(239, 68, 68, 0.08);
}

.blacklist-banner.dark,
:global(.dark) .blacklist-banner {
  background: rgba(239, 68, 68, 0.18);
  border-color: rgba(239, 68, 68, 0.4);
  border-left-color: #f87171;
  color: #fca5a5;
  box-shadow: 0 4px 12px rgba(0, 0, 0, 0.25);
}

.blacklist-info {
  display: flex;
  align-items: center;
  gap: 8px;
}

.blacklist-icon {
  font-size: 16px;
  flex-shrink: 0;
}

.blacklist-text {
  flex: 1;
  font-weight: 600;
  font-size: 12.5px;
  line-height: 1.4;
}

.blacklist-actions {
  display: flex;
  gap: 8px;
  align-items: center;
}

.unblock-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 5px 12px;
  font-size: 12px;
  font-weight: 600;
  color: #fff;
  border: none;
  border-radius: 6px;
  cursor: pointer;
  transition: all 0.15s ease;
  line-height: 1;
}

.unblock-btn.primary {
  background: #ef4444;
  flex: 1;
}

.unblock-btn.primary:hover {
  background: #dc2626;
  box-shadow: 0 2px 6px rgba(220, 38, 38, 0.4);
}

.unblock-btn.secondary {
  background: rgba(107, 114, 128, 0.9);
  flex: 1;
}

.unblock-btn.secondary:hover {
  background: rgba(75, 85, 99, 1);
}

:global(.dark) .unblock-btn.secondary {
  background: rgba(255, 255, 255, 0.18);
  color: #f3f4f6;
}

:global(.dark) .unblock-btn.secondary:hover {
  background: rgba(255, 255, 255, 0.28);
}

.unblock-btn:active {
  transform: scale(0.98);
}

/* 等级徽章（未拉黑但有降级） */
.level-badge-standalone {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  margin-top: 8px;
  padding: 4px 10px;
  background: rgba(245, 158, 11, 0.1);
  border: 1px solid rgba(245, 158, 11, 0.25);
  border-radius: 6px;
  font-size: 12px;
}

:global(.dark) .level-badge-standalone {
  background: rgba(245, 158, 11, 0.15);
  border-color: rgba(245, 158, 11, 0.35);
}

.level-hint {
  font-size: 12px;
  font-weight: 500;
  color: var(--mac-text-secondary);
}

.reset-level-mini {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 18px;
  height: 18px;
  border: none;
  background: transparent;
  color: var(--mac-text-secondary);
  cursor: pointer;
  padding: 0;
  font-size: 12px;
  border-radius: 4px;
  transition: all 0.15s;
}

.reset-level-mini:hover {
  color: #ef4444;
  background: rgba(239, 68, 68, 0.15);
}
</style>
