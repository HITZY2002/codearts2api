<script setup>
import { computed, h } from 'vue'
import UiButton from '../components/ui/UiButton.vue'
import UiCard from '../components/ui/UiCard.vue'
import UiDataTable from '../components/ui/UiDataTable.vue'
import UiSelect from '../components/ui/UiSelect.vue'
import UiTag from '../components/ui/UiTag.vue'
import {
  changeEventPage, changeEventPageSize, detailOpen, eventFilters, eventPage,
  eventPageSize, eventTotal, events, filterOptions, loading, reloadEvents, selectedLog
} from '../store'
import { eventKindLabel, eventLevelLabel, formatNumber, formatTime } from '../utils'

function openDetail(row) {
  selectedLog.value = { ...row, request_id: `事件 #${row.id}`, event: true }
  detailOpen.value = true
}

const hasActiveFilters = computed(() =>
  Object.values(eventFilters.value).some(v => v !== null && v !== undefined && v !== '')
)
function resetFilters() {
  eventFilters.value = { level: null, kind: null, account_uid: null, model: null }
  reloadEvents()
}

const levelOptions = ['info', 'warn', 'error'].map(value => ({ label: value, value }))

const eventColumns = [
  { title: '时间', key: 'created_at', width: 150, render: row => h('span', { class: 'num' }, formatTime(row.created_at)) },
  {
    title: '级别', key: 'level', width: 84,
    render: row => h(UiTag, { type: row.level === 'error' ? 'error' : row.level === 'warn' ? 'warning' : 'info' }, { default: () => eventLevelLabel(row.level) })
  },
  { title: '类型', key: 'kind', minWidth: 150, render: row => h('span', { class: 'truncate-cell', title: row.kind || '' }, eventKindLabel(row.kind)) },
  { title: '账号', key: 'account_uid', minWidth: 150, render: row => h('span', { class: 'truncate-cell', title: row.account_uid || '' }, row.account_uid || '—') },
  { title: '消息', key: 'message', minWidth: 220, className: 'wrap-cell' },
  {
    title: '数据', key: 'data', width: 72, fixed: 'right',
    render: row => h(UiButton, { size: 'xs', variant: 'outline', onClick: () => openDetail(row) }, { default: () => '查看' })
  }
]
</script>

<template>
  <div class="page">
    <UiCard title="事件" flush>
      <template #header-extra>
        <UiTag type="primary">{{ formatNumber(eventTotal) }}</UiTag>
      </template>
      <div class="filters-bar">
        <div class="filters-bar-left">
          <UiSelect v-model="eventFilters.level" clearable size="sm" placeholder="级别" :options="levelOptions" @update:model-value="reloadEvents" />
          <UiSelect v-model="eventFilters.kind" clearable filterable size="sm" placeholder="类型" :options="filterOptions('kind', events)" @update:model-value="reloadEvents" />
          <UiSelect v-model="eventFilters.account_uid" clearable filterable size="sm" placeholder="账号" :options="filterOptions('account_uid', events)" @update:model-value="reloadEvents" />
          <UiSelect v-model="eventFilters.model" clearable filterable size="sm" placeholder="模型" :options="filterOptions('model', events)" @update:model-value="reloadEvents" />
        </div>
        <div v-if="hasActiveFilters" class="filters-bar-right">
          <UiButton size="xs" variant="ghost" @click="resetFilters">重置筛选</UiButton>
        </div>
      </div>
      <UiDataTable
        :columns="eventColumns" :data="events" :loading="loading" :scroll-x="820"
        :row-key="row => row.id"
        :pagination="{ page: eventPage, pageSize: eventPageSize, itemCount: eventTotal, onUpdatePage: changeEventPage, onUpdatePageSize: changeEventPageSize }"
      />
    </UiCard>
  </div>
</template>

<style scoped>
.filters-bar .u-select { width: 152px; }
.truncate-cell { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
