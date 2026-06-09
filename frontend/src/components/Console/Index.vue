<script setup lang="ts">
import { computed, ref, onMounted, onUnmounted, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import { Call } from '@wailsio/runtime'

interface ConsoleLog {
  timestamp: string
  level: string
  message: string
}

const router = useRouter()
const logs = ref<ConsoleLog[]>([])
const autoScroll = ref(true)
const loading = ref(false)
const copyStatus = ref('')
const logsContainer = ref<HTMLElement>()
let refreshInterval: number | null = null
let copyStatusTimer: number | null = null
let loadingLogs = false
let lastLogSignature = ''

const maxRenderedLogs = 300
const maxCopyLogs = 1000

const renderedLogs = computed(() => logs.value.slice(-maxRenderedLogs))

const goBack = () => {
  router.push('/')
}

const loadLogs = async () => {
  if (loadingLogs) return
  loadingLogs = true
  try {
    const result = (await Call.ByName('codeswitch/services.ConsoleService.GetRecentLogs', maxRenderedLogs)) as ConsoleLog[]
    const signature = getLogsSignature(result)
    if (signature === lastLogSignature) return

    lastLogSignature = signature
    logs.value = result

    if (autoScroll.value) {
      await nextTick()
      scrollToBottom()
    }
  } catch (error) {
    console.error('加载控制台日志失败:', error)
  } finally {
    loadingLogs = false
  }
}

const clearLogs = async () => {
  if (!confirm('确定要清空所有控制台日志吗？')) {
    return
  }

  try {
    await Call.ByName('codeswitch/services.ConsoleService.ClearLogs')
    logs.value = []
    lastLogSignature = ''
  } catch (error) {
    console.error('清空日志失败:', error)
    alert('清空失败：' + (error as Error).message)
  }
}

const scrollToBottom = () => {
  if (logsContainer.value) {
    window.requestAnimationFrame(() => {
      if (logsContainer.value) {
        logsContainer.value.scrollTop = logsContainer.value.scrollHeight
      }
    })
  }
}

const getLogsSignature = (items: ConsoleLog[]) => {
  const last = items[items.length - 1]
  if (!last) return '0'
  return `${items.length}|${last.timestamp}|${last.level}|${last.message.length}|${last.message.slice(-80)}`
}

const formatTimestamp = (timestamp: string) => {
  const date = new Date(timestamp)
  return date.toLocaleTimeString('zh-CN', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

const getLevelClass = (level: string) => {
  switch (level.toUpperCase()) {
    case 'ERROR':
      return 'log-error'
    case 'WARN':
      return 'log-warn'
    default:
      return 'log-info'
  }
}

const formatLogLine = (log: ConsoleLog) => {
  return `[${formatTimestamp(log.timestamp)}] [${log.level}] ${log.message}`.trimEnd()
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
    throw new Error('copy failed')
  }
}

const setCopyStatus = (message: string) => {
  copyStatus.value = message
  if (copyStatusTimer) {
    window.clearTimeout(copyStatusTimer)
  }
  copyStatusTimer = window.setTimeout(() => {
    copyStatus.value = ''
    copyStatusTimer = null
  }, 1800)
}

const copyLogs = async (items: ConsoleLog[], successMessage: string) => {
  if (!items.length) return
  try {
    await copyToClipboard(items.map(formatLogLine).join('\n'))
    setCopyStatus(successMessage)
  } catch (error) {
    console.error('复制控制台日志失败:', error)
    setCopyStatus('复制失败')
  }
}

const copyVisibleLogs = async () => {
  await copyLogs(renderedLogs.value, '已复制可见日志')
}

const copyAllLogs = async () => {
  try {
    const result = (await Call.ByName('codeswitch/services.ConsoleService.GetRecentLogs', maxCopyLogs)) as ConsoleLog[]
    await copyLogs(result, '已复制全部日志')
  } catch (error) {
    console.error('读取控制台日志失败:', error)
    setCopyStatus('复制失败')
  }
}

onMounted(async () => {
  loading.value = true
  await loadLogs()
  loading.value = false

  // 每秒刷新一次日志
  refreshInterval = window.setInterval(loadLogs, 1000)
})

onUnmounted(() => {
  if (refreshInterval) {
    clearInterval(refreshInterval)
  }
  if (copyStatusTimer) {
    clearTimeout(copyStatusTimer)
  }
})
</script>

<template>
  <div class="main-shell console-shell">
    <div class="global-actions">
      <p class="global-eyebrow">控制台</p>
      <div class="actions-group">
        <button class="secondary-btn" :disabled="renderedLogs.length === 0" @click="copyVisibleLogs">复制可见</button>
        <button class="secondary-btn" :disabled="logs.length === 0" @click="copyAllLogs">复制全部</button>
        <span v-if="copyStatus" class="copy-status">{{ copyStatus }}</span>
        <button class="secondary-btn" @click="clearLogs">清空日志</button>
        <label class="auto-scroll-toggle">
          <input type="checkbox" v-model="autoScroll" />
          <span>自动滚动</span>
        </label>
        <button class="ghost-icon" aria-label="返回" @click="goBack">
          <svg viewBox="0 0 24 24" aria-hidden="true">
            <path
              d="M15 18l-6-6 6-6"
              fill="none"
              stroke="currentColor"
              stroke-width="1.5"
              stroke-linecap="round"
              stroke-linejoin="round"
            />
          </svg>
        </button>
      </div>
    </div>

    <div class="console-container">
      <div v-if="loading" class="loading-state">
        <div class="spinner"></div>
        <p>加载中...</p>
      </div>

      <div v-else class="console-content" ref="logsContainer">
        <div v-if="renderedLogs.length === 0" class="empty-state">
          <p>暂无日志</p>
        </div>

        <div
          v-for="(log, index) in renderedLogs"
          :key="`${log.timestamp}-${index}`"
          class="log-entry"
          :class="getLevelClass(log.level)"
        >
          <span class="log-timestamp">{{ formatTimestamp(log.timestamp) }}</span>
          <span class="log-level">{{ log.level }}</span>
          <span class="log-message">{{ log.message }}</span>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.console-shell {
  display: flex;
  flex-direction: column;
  height: 100%;
  overflow: hidden;
}

.actions-group {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.copy-status {
  color: var(--mac-text-secondary);
  font-size: 0.85rem;
}

.auto-scroll-toggle {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 0.9rem;
  color: var(--mac-text-secondary);
  cursor: pointer;
  user-select: none;
}

.auto-scroll-toggle input[type="checkbox"] {
  cursor: pointer;
}

.console-container {
  flex: 1;
  overflow: hidden;
  background: var(--mac-surface);
  border: 1px solid var(--mac-border);
  border-radius: 12px;
  display: flex;
  flex-direction: column;
}

.console-content {
  flex: 1;
  overflow-y: auto;
  padding: 16px;
  font-family: 'SF Mono', 'Monaco', 'Inconsolata', 'Fira Code', 'Consolas', monospace;
  font-size: 0.85rem;
  line-height: 1.6;
  background: #1e1e1e;
  color: #d4d4d4;
  contain: content;
  user-select: text;
}

html.dark .console-content {
  background: #0d1117;
  color: #e6edf3;
}

.log-entry {
  display: flex;
  gap: 12px;
  padding: 4px 0;
  border-bottom: 1px solid rgba(255, 255, 255, 0.05);
  align-items: flex-start;
  content-visibility: auto;
  contain-intrinsic-size: 24px;
  user-select: text;
}

.log-entry:last-child {
  border-bottom: none;
}

.log-timestamp {
  flex-shrink: 0;
  color: #858585;
  font-weight: 500;
}

.log-level {
  flex-shrink: 0;
  min-width: 50px;
  font-weight: 600;
}

.log-info .log-level {
  color: #4ec9b0;
}

.log-warn .log-level {
  color: #dcdcaa;
}

.log-error .log-level {
  color: #f48771;
}

.log-message {
  flex: 1;
  white-space: pre-wrap;
  word-break: break-word;
  user-select: text;
}

.loading-state,
.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  height: 100%;
  color: var(--mac-text-secondary);
}

.spinner {
  width: 32px;
  height: 32px;
  border: 3px solid rgba(0, 0, 0, 0.1);
  border-top-color: var(--mac-accent);
  border-radius: 50%;
  animation: spin 0.8s linear infinite;
  margin-bottom: 12px;
}

@keyframes spin {
  to { transform: rotate(360deg); }
}
</style>
