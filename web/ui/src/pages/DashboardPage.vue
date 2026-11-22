<script setup>
import { computed, ref } from 'vue'
import DonutChart from '../components/DonutChart.vue'
import TrendChart from '../components/TrendChart.vue'
import UiAlert from '../components/ui/UiAlert.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiCard from '../components/ui/UiCard.vue'
import UiSegmented from '../components/ui/UiSegmented.vue'
import UiStatTile from '../components/ui/UiStatTile.vue'
import {
  actionFeedback, actionLoading, metrics, observabilityDisabled,
  range, rangeOptions, reloadDashboard, runAction, series
} from '../store'
import { CHART_PALETTE, formatNumber } from '../utils'

const spark = key => series.value.map(item => item[key] ?? 0)

// ---- 性能健康 ----
const successTone = computed(() => {
  if (!metrics.value.total_requests) return 'idle'
  const rate = metrics.value.success_rate
  if (rate >= 0.95) return 'ok'
  if (rate >= 0.8) return 'warn'
  return 'bad'
})
const successBadgeTone = computed(() => {
  if (successTone.value === 'idle') return 'info'
  if (successTone.value === 'ok') return 'success'
  if (successTone.value === 'warn') return 'warning'
  return 'error'
})
const successBadgeText = computed(() => {
  if (!metrics.value.total_requests) return '暂无调用'
  return `成功率 ${(metrics.value.success_rate * 100).toFixed(1)}%`
})
const healthDotColor = computed(() => {
  if (successTone.value === 'idle') return 'var(--text-3)'
  if (successTone.value === 'ok') return 'var(--success)'
  if (successTone.value === 'warn') return 'var(--warning)'
  return 'var(--danger)'
})
const healthLabel = computed(() => {
  if (successTone.value === 'idle') return '等待调用'
  if (successTone.value === 'ok') return '运行健康'
  if (successTone.value === 'warn') return '轻微波动'
  return '异常告警'
})

const errorRateText = computed(() => {
  const total = metrics.value.total_requests || 0
  if (!total) return '错误率 0.0%'
  return `错误率 ${((metrics.value.error_count / total) * 100).toFixed(1)}%`
})

// 模型调用占比（后端 SQL 全周期 GROUP BY model 聚合，100% 精准）
const modelStats = computed(() => {
  const rows = (metrics.value.by_model || []).filter(item => item?.model && item.request_count > 0)
  const total = rows.reduce((sum, item) => sum + item.request_count, 0) || 1
  return rows.slice(0, 6).map((item, index) => ({
    model: item.model,
    count: item.request_count,
    share: item.request_count / total,
    color: CHART_PALETTE[index % CHART_PALETTE.length]
  }))
})

const donutItems = computed(() =>
  modelStats.value.map(m => ({
    label: m.model,
    value: m.count,
    share: m.share,
    color: m.color
  }))
)
const modelCallTotal = computed(() =>
  (metrics.value.by_model || []).reduce((sum, item) => sum + (item?.request_count || 0), 0)
)

// ---- Token 消耗趋势 ----
const trendLabels = computed(() =>
  series.value.map(item => {
    const d = new Date(item.bucket)
    if (range.value >= 168) {
      return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
    }
    return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  })
)

const tokenSummary = computed(() => {
  let total = 0
  let peak = 0
  for (const item of series.value) {
    const val = item.total_tokens || 0
    total += val
    if (val > peak) peak = val
  }
  const avg = series.value.length ? Math.round(total / series.value.length) : 0
  return { total, peak, avg }
})

const tokenChart = computed(() => ({
  labels: trendLabels.value,
  height: 256,
  formatValue: value => formatNumber(value),
  series: [
    { name: 'Token 消耗', type: 'line', area: true, data: series.value.map(i => i.total_tokens), color: '#6366f1' }
  ]
}))

// ---- 调用分布 ----
const distTab = ref('models') // models | latency
const distTabOptions = [
  { label: '模型占比', value: 'models' },
  { label: '延迟分布', value: 'latency' }
]

const latencyChart = computed(() => ({
  labels: trendLabels.value,
  height: 242,
  formatValue: value => (value == null ? '—' : value >= 1000 ? `${(value / 1000).toFixed(1)} s` : `${Math.round(value)} ms`),
  series: [
    { name: '平均 TTFT', type: 'line', area: true, data: series.value.map(i => i.avg_ttft_ms), color: '#0d9488' },
    { name: '平均耗时', type: 'line', data: series.value.map(i => i.avg_total_ms), color: '#6366f1' }
  ]
}))
</script>

<template>
  <div class="page">
    <div class="toolbar">
      <div class="filters">
        <UiSegmented v-model="range" :options="rangeOptions" @update:model-value="reloadDashboard" />
      </div>
      <div class="filters">
        <UiButton
          variant="outline" size="sm"
          :loading="actionLoading === '/admin/api/checkin'"
          :disabled="Boolean(actionLoading) && actionLoading !== '/admin/api/checkin'"
          @click="runAction('/admin/api/checkin')"
        >全员签到</UiButton>
        <UiButton
          variant="outline" size="sm"
          :loading="actionLoading === '/admin/api/keepalive'"
          :disabled="Boolean(actionLoading) && actionLoading !== '/admin/api/keepalive'"
          @click="runAction('/admin/api/keepalive')"
        >保活 Token</UiButton>
      </div>
    </div>

    <UiAlert v-if="actionFeedback" :type="actionFeedback.type">{{ actionFeedback.message }}</UiAlert>

    <UiAlert v-if="observabilityDisabled" type="info">
      当前服务端已关闭可观测数据库（observability.enabled = false），监控指标、请求日志与事件暂不可用，账号/模型/设置管理不受影响。
    </UiAlert>

    <div class="stat-grid">
      <UiStatTile
        label="请求总量"
        :value="formatNumber(metrics.total_requests)"
        :badge="successBadgeText"
        :badge-tone="successBadgeTone"
        caption="统计周期内网关总调用"
        tone="blue"
        icon="layers"
        :points="spark('request_count')"
      />
      <UiStatTile
        label="首字延迟 (P50)"
        :value="`${metrics.ttft_p50_ms} ms`"
        :badge="`P95 ${metrics.ttft_p95_ms} ms`"
        badge-tone="info"
        caption="流式首包响应时间"
        tone="teal"
        icon="clock"
        :points="spark('avg_ttft_ms')"
      />
      <UiStatTile
        label="Token 用量"
        :value="formatNumber(metrics.total_tokens)"
        caption="输入与输出 Token 累计"
        tone="amber"
        icon="zap"
        :points="spark('total_tokens')"
      />
      <UiStatTile
        label="异常请求"
        :value="formatNumber(metrics.error_count)"
        :badge="errorRateText"
        :badge-tone="metrics.error_count ? 'error' : 'default'"
        caption="失败或超时请求数"
        tone="rose"
        icon="x-circle"
        :points="spark('error_count')"
      />
    </div>

    <!-- 运行健康条 -->
    <div class="health-card">
      <div class="health-top">
        <div class="health-left">
          <span class="health-title">
            <span class="dot" :style="{ background: healthDotColor }" />
            {{ healthLabel }}
          </span>
          <span class="health-sep" />
          <span class="health-chip">成功率 <span class="v" :class="successTone">{{ (metrics.success_rate * 100).toFixed(2) }}%</span></span>
          <span class="health-chip">P99 延迟 <span class="v">{{ metrics.total_requests ? `${metrics.ttft_p99_ms} ms` : '—' }}</span></span>
        </div>
        <div v-if="modelStats.length" class="health-models">
          <span v-for="m in modelStats" :key="m.model" class="model-chip" :title="`统计周期内 ${m.model} 共调用 ${formatNumber(m.count)} 次`">
            <span class="dot" :style="{ background: m.color }" />
            {{ m.model }}
            <span class="pct">{{ (m.share * 100).toFixed(1) }}%</span>
          </span>
        </div>
        <span v-else class="health-chip muted">暂无模型调用分布</span>
      </div>
      <div v-if="modelStats.length" class="share-bar">
        <div
          v-for="m in modelStats" :key="m.model"
          class="share-seg"
          :style="{ width: `${Math.max(m.share * 100, 2)}%`, background: m.color }"
          :title="`${m.model}: ${(m.share * 100).toFixed(1)}% (${m.count} 次)`"
        />
      </div>
    </div>

    <!-- 核心双图：Token 消耗 + 调用分布 -->
    <div class="dash-charts">
      <UiCard title="Token 消耗" flush>
        <template #header-extra>
          <span class="muted" style="font-size: 12px">周期用量趋势</span>
        </template>
        <div class="chart-subbar">
          <span class="subbar-item">累计 <strong class="num">{{ formatNumber(tokenSummary.total) }}</strong></span>
          <span class="subbar-item">峰值/桶 <strong class="num">{{ formatNumber(tokenSummary.peak) }}</strong></span>
          <span class="subbar-item">均值/桶 <strong class="num">{{ formatNumber(tokenSummary.avg) }}</strong></span>
        </div>
        <div class="chart-pad"><TrendChart v-bind="tokenChart" /></div>
      </UiCard>

      <UiCard title="调用分布" flush>
        <template #header-extra>
          <UiSegmented v-model="distTab" :options="distTabOptions" size="sm" />
        </template>

        <DonutChart
          v-if="distTab === 'models'"
          :items="donutItems"
          center-label="周期调用"
          :center-value="formatNumber(modelCallTotal)"
        />

        <template v-else>
          <div class="chart-subbar">
            <span class="subbar-item">P50 <strong class="num">{{ metrics.ttft_p50_ms }} ms</strong></span>
            <span class="subbar-item">P95 <strong class="num">{{ metrics.ttft_p95_ms }} ms</strong></span>
            <span class="subbar-item">P99 <strong class="num">{{ metrics.ttft_p99_ms ?? '—' }} ms</strong></span>
          </div>
          <div class="chart-pad">
            <TrendChart v-bind="latencyChart" />
          </div>
        </template>
      </UiCard>
    </div>
  </div>
</template>

<style scoped>
.chart-subbar {
  display: flex; align-items: center; gap: 18px; flex-wrap: wrap;
  padding: 10px 20px;
  border-bottom: 1px solid var(--border);
  background: var(--card-2);
  font-size: 12.5px; color: var(--text-3);
}
.subbar-item { display: inline-flex; align-items: baseline; gap: 5px; }
.subbar-item strong { color: var(--text); font-weight: 650; }
.subbar-item.ok strong { color: var(--success); }
.subbar-item.err strong { color: var(--danger); }
.chart-pad { padding: 14px 16px 8px; }
.dash-charts { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px; }
@media (max-width: 1120px) {
  .dash-charts { grid-template-columns: 1fr; }
}
</style>
