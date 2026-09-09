<script setup lang="ts">
import { computed, ref } from 'vue'

/**
 * 可折叠分区：标题行（点击/Enter/Space 切换）+ 内容面板。
 * 折叠状态通过 storageKey 持久化到 localStorage（传空则不持久化）。
 * 支持 v-model:collapsed 受控模式；未受控时内部管理状态并持久化。
 */
const props = withDefaults(
  defineProps<{
    title: string
    storageKey?: string
    /** 初始是否折叠（storageKey 无缓存时生效） */
    defaultCollapsed?: boolean
  }>(),
  {
    storageKey: '',
    defaultCollapsed: true,
  },
)

const STORAGE_PREFIX = 'settings-section-collapsed-'

const readStored = (key: string): boolean | null => {
  if (!key) return null
  const cached = localStorage.getItem(STORAGE_PREFIX + key)
  return cached !== null ? cached === 'true' : null
}

const collapsed = defineModel<boolean>('collapsed', { default: undefined })

// 未受控模式：内部管理状态并持久化
const internalCollapsed = ref<boolean>(readStored(props.storageKey) ?? props.defaultCollapsed)

const isCollapsed = computed(() =>
  collapsed.value !== undefined ? collapsed.value : internalCollapsed.value,
)

const emit = defineEmits<{ toggle: [collapsed: boolean] }>()

const toggle = () => {
  const next = !isCollapsed.value
  if (collapsed.value === undefined) {
    // 未受控：更新内部状态并持久化。
    // 注意不要给 collapsed 赋值——defineModel 未绑定时是 local ref，
    // 一旦写入，undefined 判据就永远不成立，未受控分支从此不再执行。
    internalCollapsed.value = next
    if (props.storageKey) {
      localStorage.setItem(STORAGE_PREFIX + props.storageKey, String(next))
    }
  } else {
    // 受控：状态交给父组件
    collapsed.value = next
  }
  emit('toggle', next)
}
</script>

<template>
  <section :class="{ collapsed: isCollapsed }">
    <div
      class="mac-section-header"
      role="button"
      tabindex="0"
      :aria-expanded="!isCollapsed"
      @click="toggle"
      @keydown.enter.prevent="toggle"
      @keydown.space.prevent="toggle"
    >
      <h2 class="mac-section-title">{{ title }}</h2>
      <span class="section-collapse-indicator" aria-hidden="true">{{ isCollapsed ? '▸' : '▾' }}</span>
    </div>
    <div v-show="!isCollapsed" class="mac-panel collapsible-section-body">
      <slot />
    </div>
  </section>
</template>

<style scoped>
/* 面板内容内边距：避开 20px 圆角对首行首字符的裁切 */
.collapsible-section-body {
  padding: 12px 18px 14px;
}

.mac-section-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  cursor: pointer;
  padding-right: 4px;
}

.mac-section-header .mac-section-title {
  cursor: pointer;
}

.mac-section-header:focus-visible {
  outline: 2px solid var(--mac-accent);
  outline-offset: 2px;
  border-radius: 6px;
}

.section-collapse-indicator {
  color: var(--mac-text-secondary);
  font-size: 0.85rem;
  user-select: none;
  -webkit-user-select: none;
}

section.collapsed .mac-section-header {
  opacity: 0.75;
}
</style>
