<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAdaptiveHeatmap } from '../../composables/useAdaptiveHeatmap'
import type { UsageHeatmapDay } from '../../data/usageHeatmap'

defineProps<{
  visible: boolean
}>()

const { t, locale } = useI18n()

const heatmapContainerRef = ref<HTMLElement | null>(null)
const tooltipRef = ref<HTMLElement | null>(null)

const {
  displayData: usageHeatmap,
  init: initHeatmap,
  cleanup: cleanupHeatmap,
  reload: reloadHeatmap,
} = useAdaptiveHeatmap(heatmapContainerRef)

const intensityClass = (value: number) => `gh-level-${value}`

type TooltipPlacement = 'above' | 'below'

const usageTooltip = reactive({
  visible: false,
  label: '',
  dateKey: '',
  left: 0,
  top: 0,
  placement: 'above' as TooltipPlacement,
  requests: 0,
  inputTokens: 0,
  outputTokens: 0,
  reasoningTokens: 0,
  cost: 0,
})

const formatMetric = (value: number) => value.toLocaleString()

const formatTokenNumber = (value: number) => {
  if (value >= 1_000_000_000) {
    return `${(value / 1_000_000_000).toFixed(2)}B`
  }
  if (value >= 1_000_000) {
    return `${(value / 1_000_000).toFixed(2)}M`
  }
  if (value >= 1_000) {
    return `${(value / 1_000).toFixed(2)}k`
  }
  return value.toLocaleString()
}

const tooltipDateFormatter = computed(() =>
  new Intl.DateTimeFormat(locale.value || 'en', {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
)

const currencyFormatter = computed(() =>
  new Intl.NumberFormat(locale.value || 'en', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })
)

const formattedTooltipLabel = computed(() => {
  if (!usageTooltip.dateKey) return usageTooltip.label
  const date = new Date(usageTooltip.dateKey)
  if (Number.isNaN(date.getTime())) {
    return usageTooltip.label
  }
  return tooltipDateFormatter.value.format(date)
})

const formattedTooltipAmount = computed(() =>
  currencyFormatter.value.format(Math.max(usageTooltip.cost, 0))
)

const usageTooltipMetrics = computed(() => [
  {
    key: 'cost',
    label: t('components.main.heatmap.metrics.cost'),
    value: formattedTooltipAmount.value,
  },
  {
    key: 'requests',
    label: t('components.main.heatmap.metrics.requests'),
    value: formatMetric(usageTooltip.requests),
  },
  {
    key: 'inputTokens',
    label: t('components.main.heatmap.metrics.inputTokens'),
    value: formatTokenNumber(usageTooltip.inputTokens),
  },
  {
    key: 'outputTokens',
    label: t('components.main.heatmap.metrics.outputTokens'),
    value: formatTokenNumber(usageTooltip.outputTokens),
  },
  {
    key: 'reasoningTokens',
    label: t('components.main.heatmap.metrics.reasoningTokens'),
    value: formatTokenNumber(usageTooltip.reasoningTokens),
  },
])

const clamp = (value: number, min: number, max: number) => {
  if (max <= min) return min
  return Math.min(Math.max(value, min), max)
}

const TOOLTIP_DEFAULT_WIDTH = 220
const TOOLTIP_DEFAULT_HEIGHT = 120
const TOOLTIP_VERTICAL_OFFSET = 12
const TOOLTIP_HORIZONTAL_MARGIN = 20
const TOOLTIP_VERTICAL_MARGIN = 24

const getTooltipSize = () => {
  const rect = tooltipRef.value?.getBoundingClientRect()
  return {
    width: rect?.width ?? TOOLTIP_DEFAULT_WIDTH,
    height: rect?.height ?? TOOLTIP_DEFAULT_HEIGHT,
  }
}

const viewportSize = () => {
  if (typeof window !== 'undefined') {
    return { width: window.innerWidth, height: window.innerHeight }
  }
  if (typeof document !== 'undefined' && document.documentElement) {
    return {
      width: document.documentElement.clientWidth,
      height: document.documentElement.clientHeight,
    }
  }
  return {
    width: heatmapContainerRef.value?.clientWidth ?? 0,
    height: heatmapContainerRef.value?.clientHeight ?? 0,
  }
}

const showUsageTooltip = (day: UsageHeatmapDay, event: MouseEvent) => {
  const target = event.currentTarget as HTMLElement | null
  const cellRect = target?.getBoundingClientRect()
  if (!cellRect) return
  usageTooltip.label = day.label
  usageTooltip.dateKey = day.dateKey
  usageTooltip.requests = day.requests
  usageTooltip.inputTokens = day.inputTokens
  usageTooltip.outputTokens = day.outputTokens
  usageTooltip.reasoningTokens = day.reasoningTokens
  usageTooltip.cost = day.cost
  const { width: tooltipWidth, height: tooltipHeight } = getTooltipSize()
  const { width: viewportWidth, height: viewportHeight } = viewportSize()
  const centerX = cellRect.left + cellRect.width / 2
  const halfWidth = tooltipWidth / 2
  const minLeft = TOOLTIP_HORIZONTAL_MARGIN + halfWidth
  const maxLeft = viewportWidth > 0 ? viewportWidth - halfWidth - TOOLTIP_HORIZONTAL_MARGIN : centerX
  usageTooltip.left = clamp(centerX, minLeft, maxLeft)

  const anchorTop = cellRect.top
  const anchorBottom = cellRect.bottom
  const canShowAbove = anchorTop - tooltipHeight - TOOLTIP_VERTICAL_OFFSET >= TOOLTIP_VERTICAL_MARGIN
  const viewportBottomLimit = viewportHeight > 0 ? viewportHeight - tooltipHeight - TOOLTIP_VERTICAL_MARGIN : anchorBottom
  const shouldPlaceBelow = !canShowAbove
  usageTooltip.placement = shouldPlaceBelow ? 'below' : 'above'
  const desiredTop = shouldPlaceBelow
    ? anchorBottom + TOOLTIP_VERTICAL_OFFSET
    : anchorTop - tooltipHeight - TOOLTIP_VERTICAL_OFFSET
  usageTooltip.top = clamp(desiredTop, TOOLTIP_VERTICAL_MARGIN, viewportBottomLimit)
  usageTooltip.visible = true
}

const hideUsageTooltip = () => {
  usageTooltip.visible = false
}

onMounted(() => {
  initHeatmap()
})

onUnmounted(() => {
  cleanupHeatmap()
})

defineExpose({
  reload: reloadHeatmap,
})
</script>

<template>
  <section
    v-if="visible"
    ref="heatmapContainerRef"
    class="contrib-wall"
    :aria-label="t('components.main.heatmap.ariaLabel')"
  >
    <div class="contrib-legend">
      <span>{{ t('components.main.heatmap.legendLow') }}</span>
      <span v-for="level in 5" :key="level" :class="['legend-box', intensityClass(level - 1)]" />
      <span>{{ t('components.main.heatmap.legendHigh') }}</span>
    </div>

    <div class="contrib-grid">
      <div
        v-for="(week, weekIndex) in usageHeatmap"
        :key="weekIndex"
        class="contrib-column"
      >
        <div
          v-for="(day, dayIndex) in week"
          :key="dayIndex"
          class="contrib-cell"
          :class="intensityClass(day.intensity)"
          @mouseenter="showUsageTooltip(day, $event)"
          @mousemove="showUsageTooltip(day, $event)"
          @mouseleave="hideUsageTooltip"
        />
      </div>
    </div>
    <div
      v-if="usageTooltip.visible"
      ref="tooltipRef"
      class="contrib-tooltip"
      :class="usageTooltip.placement"
      :style="{ left: `${usageTooltip.left}px`, top: `${usageTooltip.top}px` }"
    >
      <p class="tooltip-heading">{{ formattedTooltipLabel }}</p>
      <ul class="tooltip-metrics">
        <li v-for="metric in usageTooltipMetrics" :key="metric.key">
          <span class="metric-label">{{ metric.label }}</span>
          <span class="metric-value">{{ metric.value }}</span>
        </li>
      </ul>
    </div>
  </section>
</template>
