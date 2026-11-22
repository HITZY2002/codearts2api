<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import UiAlert from '../components/ui/UiAlert.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiCard from '../components/ui/UiCard.vue'
import UiInput from '../components/ui/UiInput.vue'
import UiSegmented from '../components/ui/UiSegmented.vue'
import UiTag from '../components/ui/UiTag.vue'
import { request } from '../api'
import { loadAll, overview } from '../store'
import { formatNumber } from '../utils'

const search = ref('')
const kindFilter = ref('all') // all | standard | benefit
const copiedId = ref('')
const saving = ref(false)
const actionError = ref('')
const settingsSnapshot = ref(null)
let copyTimer = null

const kindOptions = [
  { label: '全部', value: 'all' },
  { label: '常规', value: 'standard' },
  { label: '限时福利', value: 'benefit' }
]

const models = computed(() => overview.value?.models || [])
const filtered = computed(() => {
  const keyword = search.value.trim().toLowerCase()
  return models.value.filter(m => {
    if (kindFilter.value === 'benefit' && !m.benefit) return false
    if (kindFilter.value === 'standard' && m.benefit) return false
    return !keyword || String(m.id).toLowerCase().includes(keyword)
  })
})

function parseHiddenList(raw) {
  const seen = new Set()
  const out = []
  for (const part of String(raw || '').split(',')) {
    const id = part.trim().toLowerCase()
    if (id && !seen.has(id)) {
      seen.add(id)
      out.push(id)
    }
  }
  return out
}

const hiddenList = computed(() => parseHiddenList(settingsSnapshot.value?.models_hidden))

async function loadSettings() {
  try {
    const res = await request('/admin/api/settings')
    settingsSnapshot.value = res.settings || null
  } catch (error) {
    actionError.value = error.message
  }
}

async function updateHiddenModels({ add = '', remove = '' }) {
  saving.value = true
  actionError.value = ''
  try {
    const latest = await request('/admin/api/settings')
    const baseSettings = latest.settings || settingsSnapshot.value
    if (!baseSettings) return
    const current = parseHiddenList(baseSettings.models_hidden)
    let nextList = current
    if (add && !current.includes(add)) {
      nextList = [...current, add]
    } else if (remove) {
      nextList = current.filter(item => item !== remove)
    }
    const nextSettings = {
      ...baseSettings,
      models_hidden: nextList.join(', ')
    }
    const res = await request('/admin/api/settings', {
      method: 'POST',
      body: JSON.stringify(nextSettings)
    })
    settingsSnapshot.value = res.settings || nextSettings
    await loadAll()
  } catch (error) {
    actionError.value = error.message
  } finally {
    saving.value = false
  }
}

function hideModel(id) {
  const target = String(id).trim().toLowerCase()
  if (!target || hiddenList.value.includes(target)) return
  updateHiddenModels({ add: target })
}

function unhideModel(id) {
  const target = String(id).trim().toLowerCase()
  if (!target) return
  updateHiddenModels({ remove: target })
}

async function copyModel(id) {
  let copied = false
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(id)
      copied = true
    } else {
      const textarea = document.createElement('textarea')
      textarea.value = id
      textarea.style.position = 'fixed'
      textarea.style.opacity = '0'
      document.body.appendChild(textarea)
      textarea.select()
      copied = document.execCommand('copy')
      document.body.removeChild(textarea)
    }
  } catch {
    copied = false
  }
  if (!copied) {
    actionError.value = '浏览器限制了剪贴板访问（非 HTTPS 环境请手动复制）'
    return
  }
  actionError.value = ''
  copiedId.value = id
  clearTimeout(copyTimer)
  copyTimer = setTimeout(() => { copiedId.value = '' }, 1400)
}

onMounted(loadSettings)
onBeforeUnmount(() => clearTimeout(copyTimer))
</script>

<template>
  <div class="page">
    <UiAlert v-if="actionError" type="error">{{ actionError }}</UiAlert>

    <UiCard title="模型列表" flush>
      <template #header-extra>
        <span class="hint muted">与 /v1/models 同源（全小写去重），点击模型 ID 可复制</span>
        <UiTag type="primary">{{ formatNumber(filtered.length) }} / {{ formatNumber(models.length) }}</UiTag>
      </template>

      <div class="filters-bar">
        <div class="filters-bar-left">
          <UiInput v-model="search" clearable size="sm" placeholder="搜索模型 ID…" style="width: 260px" />
          <UiSegmented v-model="kindFilter" :options="kindOptions" size="sm" />
        </div>
      </div>

      <div v-if="filtered.length" class="models-grid">
        <div
          v-for="m in filtered" :key="m.id"
          class="model-card" :class="{ 'is-copied': copiedId === m.id }"
        >
          <button type="button" class="model-main" :title="`点击复制 ${m.id}`" @click="copyModel(m.id)">
            <span class="mono model-id">{{ m.id }}</span>
            <UiTag v-if="copiedId === m.id" type="success">已复制</UiTag>
            <UiTag v-else-if="m.benefit" type="warning">福利</UiTag>
          </button>
          <UiButton
            size="xs" variant="ghost" :disabled="saving"
            title="从 /v1/models 中隐藏该模型"
            @click="hideModel(m.id)"
          >隐藏</UiButton>
        </div>
      </div>
      <div v-else class="empty-line">暂无匹配的模型</div>
    </UiCard>

    <UiCard v-if="hiddenList.length" title="已隐藏模型">
      <template #header-extra>
        <span class="hint muted">以下模型已从 /v1/models 接口中隐藏</span>
        <UiTag>{{ hiddenList.length }}</UiTag>
      </template>
      <div class="hidden-list">
        <div v-for="id in hiddenList" :key="id" class="hidden-chip">
          <span class="mono">{{ id }}</span>
          <UiButton size="xs" variant="outline" :disabled="saving" @click="unhideModel(id)">恢复</UiButton>
        </div>
      </div>
    </UiCard>
  </div>
</template>

<style scoped>
.hint { font-size: 12px; }
.models-grid {
  display: grid; grid-template-columns: repeat(auto-fill, minmax(290px, 1fr));
  gap: 10px; padding: 16px 18px 18px;
}
.model-card {
  display: flex; align-items: center; justify-content: space-between; gap: 8px;
  border: 1px solid var(--border); border-radius: 10px;
  padding: 8px 10px 8px 14px; background: var(--card-2); min-width: 0;
  transition: border-color 0.15s, background 0.15s;
}
.model-card:hover { border-color: var(--border-strong); }
.model-card.is-copied { border-color: var(--success); }
.model-main {
  flex: 1; min-width: 0;
  display: flex; align-items: center; gap: 8px;
  border: none; background: transparent; padding: 3px 0;
  cursor: pointer; text-align: left; font-family: inherit;
}
.model-id { color: var(--text); font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.hidden-list { display: flex; flex-wrap: wrap; gap: 10px; }
.hidden-chip {
  display: inline-flex; align-items: center; gap: 10px;
  padding: 6px 8px 6px 12px;
  border-radius: 9px; border: 1px solid var(--border);
  background: var(--card-2);
}
.empty-line { padding: 40px 0; text-align: center; color: var(--text-3); font-size: 13px; }
</style>
