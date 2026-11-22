<script setup>
// 手写 SVG 趋势图：折线/面积 + 柱状，可选双 Y 轴，悬停十字线 + 提示框，图例置底。
// 替代 echarts + vue-echarts（两个依赖约 500KB raw / 130KB gzip）。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

const props = defineProps({
  labels: { type: Array, default: () => [] },
  series: { type: Array, default: () => [] }, // { name, type: 'line'|'bar', data, yAxisIndex?, area?, color? }
  height: { type: Number, default: 280 },
  dualAxis: Boolean,
  formatValue: { type: Function, default: value => Number(value).toLocaleString() }
})

const PALETTE = ['#2dd4bf', '#8b5cf6', '#f59e0b', '#3b82f6', '#ec4899', '#22c55e']
const gradId = `cg-${Math.random().toString(36).slice(2, 9)}`

const box = ref(null)
const svgEl = ref(null)
const width = ref(640)
const hover = ref(-1)
let ro = null

onMounted(() => {
  ro = new ResizeObserver(entries => {
    const w = entries[0]?.contentRect?.width
    if (w) width.value = Math.max(220, Math.round(w))
  })
  if (box.value) ro.observe(box.value)
})
onBeforeUnmount(() => ro?.disconnect())

const geom = computed(() => {
  const m = { top: 16, right: props.dualAxis ? 50 : 16, bottom: 26, left: 46 }
  const plotW = Math.max(10, width.value - m.left - m.right)
  const plotH = Math.max(10, props.height - m.top - m.bottom)
  const n = props.labels.length
  return { w: width.value, h: props.height, m, plotW, plotH, n, step: n > 1 ? plotW / (n - 1) : 0 }
})

function niceMax(value) {
  if (!Number.isFinite(value) || value <= 0) return 4
  const rough = value / 4
  const mag = Math.pow(10, Math.floor(Math.log10(rough)))
  const norm = rough / mag
  const step = (norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 2.5 ? 2.5 : norm <= 5 ? 5 : 10) * mag
  return 4 * step
}
function maxOf(list) {
  let mx = 0
  for (const s of list) for (const v of s.data) if (v != null && Number.isFinite(v) && v > mx) mx = v
  return mx
}

const y1Max = computed(() => niceMax(maxOf(props.series.filter(s => (s.yAxisIndex || 0) === 0))))
const y2Max = computed(() => (props.dualAxis ? niceMax(maxOf(props.series.filter(s => s.yAxisIndex === 1))) : 1))

const y1Ticks = computed(() => [0, 1, 2, 3, 4].map(i => (y1Max.value / 4) * i))
const y2Ticks = computed(() => [0, 1, 2, 3, 4].map(i => (y2Max.value / 4) * i))

const xAt = i => {
  const { m, plotW, n, step } = geom.value
  return m.left + (n > 1 ? i * step : plotW / 2)
}
const y1At = v => geom.value.m.top + geom.value.plotH * (1 - (v || 0) / y1Max.value)
const y2At = v => geom.value.m.top + geom.value.plotH * (1 - (v || 0) / y2Max.value)

const r2 = v => Math.round(v * 100) / 100

function smoothPath(pts) {
  if (!pts.length) return ''
  if (pts.length === 1) return `M${pts[0][0]},${pts[0][1]}`
  let d = `M${pts[0][0]},${pts[0][1]}`
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[i - 1] || pts[i]
    const p1 = pts[i]
    const p2 = pts[i + 1]
    const p3 = pts[i + 2] || p2
    const c1x = p1[0] + (p2[0] - p0[0]) / 6
    const c1y = p1[1] + (p2[1] - p0[1]) / 6
    const c2x = p2[0] - (p3[0] - p1[0]) / 6
    const c2y = p2[1] - (p3[1] - p1[1]) / 6
    d += ` C${r2(c1x)},${r2(c1y)} ${r2(c2x)},${r2(c2y)} ${p2[0]},${p2[1]}`
  }
  return d
}

// 折线 / 面积系列
const lineSeries = computed(() => {
  const { m, plotH } = geom.value
  const baseY = m.top + plotH
  return props.series
    .filter(s => s.type !== 'bar')
    .map((s, index) => {
      const yFn = s.yAxisIndex === 1 ? y2At : y1At
      const pts = s.data.map((v, i) => [r2(xAt(i)), r2(yFn(v))])
      const linePath = smoothPath(pts)
      const first = pts[0]
      const last = pts[pts.length - 1]
      const areaPath = pts.length && s.area ? `${linePath} L${last[0]},${r2(baseY)} L${first[0]},${r2(baseY)} Z` : ''
      return { ...s, color: s.color || PALETTE[index % PALETTE.length], linePath, areaPath, pts }
    })
})
const areaColor = computed(() => lineSeries.value.find(s => s.area)?.color || lineSeries.value[0]?.color || PALETTE[0])

// 柱状系列（多系列并排分组）
const barRects = computed(() => {
  const bars = props.series.filter(s => s.type === 'bar')
  const { n, step, plotW } = geom.value
  if (!bars.length || !n) return []
  const band = n > 1 ? step : plotW
  const groupW = Math.min(band * 0.5, 30)
  const barW = groupW / bars.length
  const baseY = geom.value.m.top + geom.value.plotH
  const rects = []
  bars.forEach((s, groupIndex) => {
    const color = s.color || PALETTE[(lineSeries.value.length + groupIndex) % PALETTE.length]
    s.data.forEach((v, i) => {
      if (v == null || !Number.isFinite(v)) return
      const hPix = Math.max(0, baseY - y1At(v))
      rects.push({
        x: r2(xAt(i) - groupW / 2 + groupIndex * barW),
        y: r2(y1At(v)),
        w: r2(barW - Math.min(2, barW * 0.12)),
        h: r2(hPix),
        color
      })
    })
  })
  return rects
})

// x 轴标签抽样：末尾若与最后一个抽样点过近则替换，避免边缘标签重叠。
const xLabelList = computed(() => {
  const { n } = geom.value
  if (!n) return []
  const labelStep = Math.max(1, Math.ceil(n / 9))
  const out = []
  for (let i = 0; i < n; i += labelStep) out.push({ i, text: props.labels[i] })
  const last = out[out.length - 1]
  if (!last || last.i === n - 1) return out
  if (n - 1 - last.i < labelStep) last.text = props.labels[n - 1], (last.i = n - 1)
  else out.push({ i: n - 1, text: props.labels[n - 1] })
  return out
})

function fmtTick(v) {
  const abs = Math.abs(v)
  if (abs >= 1e8) return `${Number((v / 1e8).toFixed(1))}亿`
  if (abs >= 1e4) return `${Number((v / 1e4).toFixed(1))}万`
  if (abs >= 1000) return `${Number((v / 1000).toFixed(1))}k`
  return `${Number(v.toFixed(1))}`
}

function onMove(event) {
  const { n, m, step, plotW } = geom.value
  if (!n || !svgEl.value) return
  const rect = svgEl.value.getBoundingClientRect()
  const rel = event.clientX - rect.left - m.left
  let idx = n > 1 ? Math.round(rel / step) : 0
  idx = Math.max(0, Math.min(n - 1, idx))
  if (rel < -8 || rel > plotW + 8) hover.value = -1
  else hover.value = idx
}

const tipStyle = computed(() => {
  if (hover.value < 0) return {}
  const x = xAt(hover.value)
  const left = Math.min(Math.max(x, 80), width.value - 84)
  const ys = props.series
    .map(s => (s.yAxisIndex === 1 ? y2At : y1At)(s.data[hover.value]))
    .filter(v => Number.isFinite(v))
  const top = (ys.length ? Math.min(...ys) : geom.value.m.top) + 6
  return { left: `${left}px`, top: `${top}px`, transform: 'translate(-50%, 0)' }
})
const tipRows = computed(() => {
  if (hover.value < 0) return []
  return props.series.map(s => ({
    name: s.name,
    color: s.color || PALETTE[props.series.indexOf(s) % PALETTE.length],
    value: props.formatValue(s.data[hover.value], s.name)
  }))
})
</script>

<template>
  <div ref="box" class="chart-box" :style="{ height: `${height}px` }">
    <svg v-if="geom.n" ref="svgEl" :width="geom.w" :height="height" @mousemove="onMove" @mouseleave="hover = -1">
      <defs>
        <linearGradient :id="gradId" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" :stop-color="areaColor" stop-opacity="0.2" />
          <stop offset="1" :stop-color="areaColor" stop-opacity="0" />
        </linearGradient>
      </defs>

      <!-- 横向网格 + Y 轴刻度 -->
      <g>
        <template v-for="(t, i) in y1Ticks" :key="`g${i}`">
          <line
            :x1="geom.m.left" :x2="geom.w - geom.m.right" :y1="r2(y1At(t))" :y2="r2(y1At(t))"
            :stroke="i === 0 ? 'var(--border-strong)' : 'var(--border)'"
            :stroke-dasharray="i === 0 ? undefined : '3 4'"
            stroke-width="1"
          />
          <text :x="geom.m.left - 8" :y="r2(y1At(t) + 3.5)" text-anchor="end" font-size="10.5" style="fill: var(--text-3)">{{ fmtTick(t) }}</text>
        </template>
        <template v-if="dualAxis">
          <text
            v-for="(t, i) in y2Ticks" :key="`g2${i}`"
            :x="geom.w - geom.m.right + 8" :y="r2(y2At(t) + 3.5)" text-anchor="start"
            font-size="10.5" style="fill: var(--text-3)"
          >{{ fmtTick(t) }}</text>
        </template>
      </g>

      <!-- X 轴标签 -->
      <text
        v-for="item in xLabelList" :key="`x${item.i}`"
        :x="r2(xAt(item.i))" :y="geom.h - 8" text-anchor="middle" font-size="10.5" style="fill: var(--text-3)"
      >{{ item.text }}</text>

      <!-- 悬停十字线 -->
      <line
        v-if="hover >= 0"
        :x1="r2(xAt(hover))" :x2="r2(xAt(hover))" :y1="geom.m.top" :y2="geom.m.top + geom.plotH"
        stroke="var(--border-strong)" stroke-dasharray="3 3"
      />

      <!-- 柱状系列 -->
      <rect v-for="(rect, i) in barRects" :key="`b${i}`" :x="rect.x" :y="rect.y" :width="rect.w" :height="rect.h" :fill="rect.color" rx="4" />

      <!-- 面积 -->
      <template v-for="s in lineSeries" :key="`a${s.name}`">
        <path v-if="s.area" :d="s.areaPath" :fill="`url(#${gradId})`" />
      </template>

      <!-- 折线 -->
      <path
        v-for="s in lineSeries" :key="`l${s.name}`" :d="s.linePath" fill="none"
        :stroke="s.color" stroke-width="2" stroke-linecap="round" :opacity="hover >= 0 ? 0.92 : 1"
      />

      <!-- 悬停数据点 -->
      <g v-if="hover >= 0">
        <circle
          v-for="s in lineSeries" :key="`p${s.name}`"
          :cx="s.pts[hover]?.[0]" :cy="s.pts[hover]?.[1]" r="3.2"
          :fill="s.color" stroke="var(--card)" stroke-width="1.5"
        />
      </g>
    </svg>
    <div v-else class="chart-empty">暂无数据</div>

    <div v-if="hover >= 0 && geom.n" class="chart-tip" :style="tipStyle">
      <div class="tip-title">{{ labels[hover] }}</div>
      <div v-for="row in tipRows" :key="row.name" class="tip-row">
        <span class="l"><span class="dot" :style="{ background: row.color }" />{{ row.name }}</span>
        <span class="v">{{ row.value }}</span>
      </div>
    </div>
  </div>

  <div v-if="series.length" class="chart-legend">
    <span v-for="(s, i) in series" :key="s.name" class="item">
      <span class="dot" :style="{ background: s.color || PALETTE[i % PALETTE.length] }" />{{ s.name }}
    </span>
  </div>
</template>
