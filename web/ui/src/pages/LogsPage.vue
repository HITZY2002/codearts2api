<script setup>
import { computed, h } from 'vue'
import UiAlert from '../components/ui/UiAlert.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiCard from '../components/ui/UiCard.vue'
import UiDataTable from '../components/ui/UiDataTable.vue'
import UiSelect from '../components/ui/UiSelect.vue'
import UiTag from '../components/ui/UiTag.vue'
import {
  changeLogPage, changeLogPageSize, detailOpen, filterOptions, loading, logFilters,
  logPage, logPageSize, logTotal, logs, observabilityDisabled, reloadLogs, selectedLog
} from '../store'
import { formatDuration, formatNumber, formatTime, latencyTone, statusType } from '../utils'

function openDetail(row) {
  selectedLog.value = row
  detailOpen.value = true
}

const hasActiveFilters = computed(() =>
  Object.values(logFilters.value).some(v => v !== null && v !== undefined && v !== '')
)
function resetFilters() {
  logFilters.value = { status_code: null, model: null, account_uid: null, error_type: null, stream: null }
  reloadLogs()
}

const statusOptions = [200, 400, 401, 403, 429, 500, 502, 503, 504].map(value => ({ label: String(value), value }))
const streamOptions = [{ label: '流式', value: true }, { label: '非流式', value: false }]
const creditFormatter = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 6 })
function formatCreditCost(value) {
  if (value == null) return '—'
  const num = Number(value)
  if (!Number.isFinite(num)) return String(value)
  return creditFormatter.format(num)
}

const logColumns = [
  { title: '时间', key: 'created_at', width: 172, render: row => h('span', { class: 'num' }, formatTime(row.created_at)) },
  { title: '模型', key: 'model', minWidth: 150, render: row => h('span', { class: 'truncate-cell', title: row.model || '' }, row.model || '—') },
  { title: '账号', key: 'account_name', minWidth: 130, render: row => h('span', { class: 'truncate-cell', title: row.account_name || '' }, row.account_name || '—') },
  { title: '流', key: 'stream', width: 78, render: row => h(UiTag, { type: row.stream ? 'info' : 'default' }, { default: () => (row.stream ? '流' : '非流') }) },
  { title: '状态', key: 'status_code', width: 68, render: row => h(UiTag, { type: statusType(row.status_code) }, { default: () => String(row.status_code) }) },
  { title: 'TTFT', key: 'first_output_ms', width: 90, align: 'right', render: row => h('span', { class: 'num' }, row.first_output_ms == null ? '—' : formatDuration(row.first_output_ms)) },
  {
    title: '耗时', key: 'total_ms', width: 104,
    render: row => h('div', { class: 'lat-bar' }, [
      h('span', { class: 'bar', style: { background: latencyTone(row.total_ms) } }),
      h('span', { class: 'texts' }, [h('span', { class: 'main' }, formatDuration(row.total_ms))])
    ])
  },
  { title: 'Token', key: 'total_tokens', width: 100, align: 'right', render: row => h('span', { class: 'num' }, formatNumber(row.total_tokens)) },
  { title: '积分', key: 'credit_cost', width: 88, align: 'right', render: row => h('span', { class: 'num' }, formatCreditCost(row.credit_cost)) },
  { title: '上游消息 ID', key: 'upstream_msg_id', minWidth: 170, render: row => h('span', { class: 'mono truncate-cell', title: row.upstream_msg_id || '' }, row.upstream_msg_id || '—') },
  { title: 'IP', key: 'client_ip', width: 118 },
  {
    title: '详情', key: 'details', width: 66, fixed: 'right',
    render: row => h(UiButton, { size: 'xs', variant: 'outline', onClick: () => openDetail(row) }, { default: () => '查看' })
  }
]
</script>

<template>
  <div class="page">
    <UiCard title="请求日志" flush>
      <template #header-extra>
        <UiTag type="primary">{{ formatNumber(logTotal) }}</UiTag>
      </template>
      <div v-if="observabilityDisabled" style="padding: 14px 18px 0">
        <UiAlert type="info">当前服务端已关闭可观测数据库（observability.enabled = false），请求日志不可用。</UiAlert>
      </div>
      <div class="filters-bar">
        <div class="filters-bar-left">
          <UiSelect v-model="logFilters.status_code" clearable size="sm" placeholder="状态码" :options="statusOptions" @update:model-value="reloadLogs" />
          <UiSelect v-model="logFilters.model" clearable filterable size="sm" placeholder="模型" :options="filterOptions('model', logs, logFilters.model)" @update:model-value="reloadLogs" />
          <UiSelect v-model="logFilters.account_uid" clearable filterable size="sm" placeholder="账号" :options="filterOptions('account_uid', logs, logFilters.account_uid)" @update:model-value="reloadLogs" />
          <UiSelect v-model="logFilters.error_type" clearable size="sm" placeholder="错误类型" :options="filterOptions('error_type', logs, logFilters.error_type)" @update:model-value="reloadLogs" />
          <UiSelect v-model="logFilters.stream" clearable size="sm" placeholder="流式" :options="streamOptions" @update:model-value="reloadLogs" />
        </div>
        <div v-if="hasActiveFilters" class="filters-bar-right">
          <UiButton size="xs" variant="ghost" @click="resetFilters">重置筛选</UiButton>
        </div>
      </div>
      <UiDataTable
        :columns="logColumns" :data="logs" :loading="loading" :scroll-x="1220"
        :row-key="row => row.request_id"
        :pagination="{ page: logPage, pageSize: logPageSize, itemCount: logTotal, onUpdatePage: changeLogPage, onUpdatePageSize: changeLogPageSize }"
      />
    </UiCard>
  </div>
</template>

<style scoped>
.filters-bar .u-select { width: 152px; }
.truncate-cell { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
