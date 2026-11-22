<script setup>
import UiDrawer from '../components/ui/UiDrawer.vue'
import { detailOpen, selectedLog } from '../store'
</script>

<template>
  <UiDrawer v-model="detailOpen" title="记录详情" :width="560">
    <div v-if="selectedLog" class="detail-list">
      <div v-for="(value, key) in selectedLog" :key="key" class="detail-row">
        <span class="k">{{ key }}</span>
        <span class="v">
          <pre v-if="key === 'data'" class="detail-json">{{ typeof value === 'string' ? value : JSON.stringify(value, null, 2) }}</pre>
          <template v-else>{{ value == null || value === '' ? '—' : String(value) }}</template>
        </span>
      </div>
    </div>
  </UiDrawer>
</template>
