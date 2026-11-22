<script setup>
import { computed } from 'vue'
import Icon from './Icon.vue'
import UiTag from './UiTag.vue'

const props = defineProps({
  label: String,
  value: { type: [String, Number], default: '' },
  caption: String,
  badge: String,
  badgeTone: { type: String, default: 'default' },
  tone: { type: String, default: 'blue' }, // blue | teal | green | amber | rose | violet
  icon: String,
  points: { type: Array, default: null }
})

const gradId = `sp-${Math.random().toString(36).slice(2, 8)}`

const sparkGeom = computed(() => {
  const pts = props.points
  if (!pts || pts.length < 2) return null
  const w = 100
  const h = 30
  const min = Math.min(...pts)
  const max = Math.max(...pts)
  const span = max - min || 1
  const coords = pts.map((v, i) => [
    ((i / (pts.length - 1)) * w).toFixed(1),
    (h - 3 - ((v - min) / span) * (h - 6)).toFixed(1)
  ])
  const line = coords.map(([x, y], i) => `${i ? 'L' : 'M'}${x},${y}`).join(' ')
  const area = `${line} L${w},${h} L0,${h} Z`
  return { line, area }
})
</script>

<template>
  <div class="stat-tile">
    <div class="stat-tile-top">
      <span class="stat-tile-label">{{ label }}</span>
      <span v-if="icon" class="stat-ico" :class="tone"><Icon :name="icon" :size="15" /></span>
    </div>
    <div class="stat-value-row">
      <span class="stat-value">{{ value }}</span>
      <UiTag v-if="badge" :type="badgeTone">{{ badge }}</UiTag>
    </div>
    <div v-if="caption" class="stat-caption">{{ caption }}</div>
    <svg v-if="sparkGeom" class="stat-spark" :class="tone" width="100%" height="30" viewBox="0 0 100 30" preserveAspectRatio="none">
      <defs>
        <linearGradient :id="gradId" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="currentColor" stop-opacity="0.22" />
          <stop offset="100%" stop-color="currentColor" stop-opacity="0" />
        </linearGradient>
      </defs>
      <path :d="sparkGeom.area" :fill="`url(#${gradId})`" />
      <path :d="sparkGeom.line" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" vector-effect="non-scaling-stroke" />
    </svg>
  </div>
</template>

<style scoped>
.stat-spark { display: block; color: var(--text-3); }
.stat-spark.blue { color: var(--primary); }
.stat-spark.teal { color: var(--accent); }
.stat-spark.green { color: var(--success); }
.stat-spark.amber { color: var(--warning); }
.stat-spark.rose { color: var(--danger); }
.stat-spark.violet { color: #8b5cf6; }
html.theme-dark .stat-spark.violet { color: #a78bfa; }
</style>
