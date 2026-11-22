<script setup>
import { computed, ref } from 'vue'
import { formatNumber } from '../utils'

const props = defineProps({
  items: { type: Array, default: () => [] }, // [{ label, value, share, color }]
  centerLabel: { type: String, default: '总调用' },
  centerValue: { type: [String, Number], default: 0 }
})

const hoverIdx = ref(-1)

const segments = computed(() => {
  const total = props.items.reduce((sum, item) => sum + (item.value || 0), 0) || 1
  const r = 62
  const c = 2 * Math.PI * r
  let offset = 0
  return props.items.map((item, idx) => {
    const ratio = (item.value || 0) / total
    const len = ratio * c
    const gap = props.items.length > 1 ? Math.min(3, len * 0.15) : 0
    const seg = {
      ...item,
      share: ratio,
      idx,
      dashArray: `${Math.max(0, len - gap)} ${c}`,
      dashOffset: -offset
    }
    offset += len
    return seg
  })
})

const activeItem = computed(() => (hoverIdx.value >= 0 ? segments.value[hoverIdx.value] : null))
</script>

<template>
  <div v-if="items.length" class="donut-layout">
    <div class="donut-ring-wrap">
      <svg class="donut-svg" width="164" height="164" viewBox="0 0 164 164">
        <circle cx="82" cy="82" r="62" fill="none" stroke="var(--canvas-2)" stroke-width="18" />
        <circle
          v-for="seg in segments" :key="seg.label"
          cx="82" cy="82" r="62"
          fill="none"
          :stroke="seg.color"
          :stroke-width="hoverIdx === seg.idx ? 22 : 18"
          stroke-linecap="round"
          :stroke-dasharray="seg.dashArray"
          :stroke-dashoffset="seg.dashOffset"
          :opacity="hoverIdx >= 0 && hoverIdx !== seg.idx ? 0.45 : 1"
          transform="rotate(-90 82 82)"
          class="donut-seg"
          @mouseenter="hoverIdx = seg.idx"
          @mouseleave="hoverIdx = -1"
        />
      </svg>
      <div class="donut-center">
        <span class="donut-center-val num">
          {{ activeItem ? formatNumber(activeItem.value) : centerValue }}
        </span>
        <span class="donut-center-lbl" :title="activeItem?.label || centerLabel">
          {{ activeItem ? `${(activeItem.share * 100).toFixed(1)}%` : centerLabel }}
        </span>
      </div>
    </div>

    <div class="donut-list">
      <div
        v-for="seg in segments" :key="seg.label"
        class="donut-row" :class="{ 'is-active': hoverIdx === seg.idx }"
        @mouseenter="hoverIdx = seg.idx"
        @mouseleave="hoverIdx = -1"
      >
        <div class="donut-row-head">
          <span class="donut-name">
            <span class="dot" :style="{ background: seg.color }" />
            <span class="mono" :title="seg.label">{{ seg.label }}</span>
          </span>
          <span class="donut-meta num">
            <strong>{{ formatNumber(seg.value) }}</strong>
            <span class="pct">{{ (seg.share * 100).toFixed(1) }}%</span>
          </span>
        </div>
        <div class="donut-track">
          <div class="donut-fill" :style="{ width: `${Math.max(seg.share * 100, 2)}%`, background: seg.color }" />
        </div>
      </div>
    </div>
  </div>
  <div v-else class="chart-empty" style="position: static; height: 260px">暂无调用分布数据</div>
</template>

<style scoped>
.donut-layout {
  display: grid;
  grid-template-columns: 172px minmax(0, 1fr);
  align-items: center;
  gap: 22px;
  padding: 18px 20px 20px;
  min-height: 292px;
}
.donut-ring-wrap {
  position: relative;
  width: 164px; height: 164px;
  margin: 0 auto;
}
.donut-svg { display: block; overflow: visible; }
.donut-seg { transition: stroke-width 0.16s ease, opacity 0.16s ease; cursor: pointer; }
.donut-center {
  position: absolute; inset: 0;
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  pointer-events: none; text-align: center; padding: 0 22px;
}
.donut-center-val { font-size: 22px; font-weight: 750; letter-spacing: -0.02em; color: var(--text); line-height: 1.15; }
.donut-center-lbl {
  font-size: 11.5px; color: var(--text-3); margin-top: 3px;
  max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.donut-list { display: grid; gap: 11px; min-width: 0; }
.donut-row {
  display: grid; gap: 5px;
  padding: 5px 8px; border-radius: 8px;
  transition: background 0.14s ease;
  cursor: default;
}
.donut-row.is-active { background: var(--card-2); }
.donut-row-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; min-width: 0; }
.donut-name { display: flex; align-items: center; gap: 8px; min-width: 0; color: var(--text); }
.donut-name .mono { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12.5px; }
.donut-name .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
.donut-meta { display: flex; align-items: baseline; gap: 8px; flex: none; font-size: 12.5px; }
.donut-meta strong { color: var(--text); font-weight: 650; }
.donut-meta .pct { color: var(--text-3); font-size: 11.5px; min-width: 42px; text-align: right; }
.donut-track { height: 6px; border-radius: 999px; background: var(--canvas-2); overflow: hidden; }
.donut-fill { height: 100%; border-radius: 999px; transition: width 0.3s ease; }

@media (max-width: 600px) {
  .donut-layout { grid-template-columns: 1fr; }
}
</style>
