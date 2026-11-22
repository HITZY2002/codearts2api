<script setup>
import { computed } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  page: { type: Number, required: true },
  pageSize: { type: Number, required: true },
  itemCount: { type: Number, default: 0 },
  pageSizeOptions: { type: Array, default: () => [20, 50, 100] }
})

const emit = defineEmits(['update:page', 'update:pageSize'])

const totalPages = computed(() => Math.max(1, Math.ceil(props.itemCount / props.pageSize)))

const pageList = computed(() => {
  const total = totalPages.value
  const cur = Math.min(props.page, total)
  const wanted = new Set([1, total, cur - 1, cur, cur + 1].filter(p => p >= 1 && p <= total))
  const sorted = [...wanted].sort((a, b) => a - b)
  const out = []
  let prev = 0
  for (const p of sorted) {
    if (p - prev > 1) out.push('…')
    out.push(p)
    prev = p
  }
  return out
})
</script>

<template>
  <div class="u-pagination">
    <span class="page-size-label">总计 {{ itemCount }}</span>
    <span class="page-size-label">每页</span>
    <select
      class="page-size-select" :value="pageSize"
      @change="emit('update:pageSize', Number($event.target.value))"
    >
      <option v-for="size in pageSizeOptions" :key="size" :value="size">{{ size }}</option>
    </select>
    <div class="pages">
      <button class="page-btn" :disabled="page <= 1" title="上一页" @click="emit('update:page', page - 1)">
        <Icon name="chevron-left" :size="14" />
      </button>
      <template v-for="(p, i) in pageList" :key="`${p}-${i}`">
        <span v-if="p === '…'" class="page-ellipsis">…</span>
        <button v-else class="page-btn" :class="{ 'is-current': p === page }" @click="emit('update:page', p)">{{ p }}</button>
      </template>
      <button class="page-btn" :disabled="page >= totalPages" title="下一页" @click="emit('update:page', page + 1)">
        <Icon name="chevron-right" :size="14" />
      </button>
    </div>
  </div>
</template>

<style scoped>
.page-size-select {
  height: 26px; padding: 0 6px;
  background: var(--card); border: 1px solid var(--border-strong); border-radius: 7px;
  color: var(--text); font-size: 12px; font-family: inherit;
}
html.theme-dark .page-size-select { background: var(--card-2); border-color: var(--border); }
.page-size-select:focus { outline: none; border-color: var(--primary); }
.page-ellipsis { color: var(--text-3); padding: 0 3px; }
</style>
