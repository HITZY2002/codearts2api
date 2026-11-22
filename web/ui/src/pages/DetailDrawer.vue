<script setup>
import UiDrawer from '../components/ui/UiDrawer.vue'
import { detailOpen, selectedLog } from '../store'
import { eventKindLabel, eventLevelLabel, formatTime } from '../utils'

function formatField(key, value) {
  if (value == null || value === '') return '—'
  if (key === 'created_at') return formatTime(value)
  if (key === 'kind') return `${eventKindLabel(value)} (${value})`
  if (key === 'level') return `${eventLevelLabel(value)} (${value})`
  return String(value)
}
</script>

<template>
  <UiDrawer v-model="detailOpen" title="记录详情" :width="560">
    <div v-if="selectedLog" class="detail-list">
      <div v-for="(value, key) in selectedLog" :key="key" class="detail-row">
        <span class="k">{{ key }}</span>
        <span class="v">
          <pre v-if="key === 'data'" class="detail-json">{{ typeof value === 'string' ? value : JSON.stringify(value, null, 2) }}</pre>
          <template v-else>{{ formatField(key, value) }}</template>
        </span>
      </div>
    </div>
  </UiDrawer>
</template>
