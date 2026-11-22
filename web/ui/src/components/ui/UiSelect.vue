<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

const props = defineProps({
  options: { type: Array, default: () => [] }, // [{ label, value }]
  clearable: Boolean,
  filterable: Boolean,
  placeholder: { type: String, default: '请选择' },
  size: { type: String, default: 'md' }, // md | sm
  disabled: Boolean
})

const model = defineModel()
const root = ref(null)
const searchBox = ref(null)
const open = ref(false)
const query = ref('')

const current = computed(() => props.options.find(option => option.value === model.value))
const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return props.options
  return props.options.filter(option => String(option.label).toLowerCase().includes(q))
})
const showClear = computed(() => props.clearable && model.value !== null && model.value !== undefined && model.value !== '')

function openMenu() {
  if (props.disabled) return
  query.value = ''
  open.value = true
  nextTick(() => searchBox.value?.focus())
}
function close() { open.value = false }
function toggle() { open.value ? close() : openMenu() }
function pick(option) {
  model.value = option.value
  close()
  root.value?.focus()
}
function clear(event) {
  event.stopPropagation()
  model.value = null
}
function onDocMousedown(event) {
  if (open.value && root.value && !root.value.contains(event.target)) close()
}
function onKeydown(event) {
  if (event.key === 'Escape') {
    if (open.value) close()
    else if (props.clearable) model.value = null
  } else if (event.key === 'Enter' && open.value && filtered.value.length) {
    pick(filtered.value[0])
  }
}

onMounted(() => document.addEventListener('mousedown', onDocMousedown))
onBeforeUnmount(() => document.removeEventListener('mousedown', onDocMousedown))
watch(() => props.disabled, value => { if (value) close() })
</script>

<template>
  <div
    ref="root" class="u-select"
    :class="[`u-select--${size}`, { 'is-open': open, 'is-disabled': disabled }]"
    tabindex="0" @keydown="onKeydown"
  >
    <div class="u-select-trigger" :style="showClear ? { paddingRight: '44px' } : null" @click="toggle">
      <span v-if="current" class="u-select-label">{{ current.label }}</span>
      <span v-else class="u-select-placeholder">{{ placeholder }}</span>
      <span v-if="showClear" class="caret-clear" @click.stop="clear">×</span>
      <span class="caret">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6" /></svg>
      </span>
    </div>
    <Transition name="pop">
      <div v-if="open" class="u-select-menu">
        <input v-if="filterable" ref="searchBox" v-model="query" class="u-select-search" placeholder="搜索…" @click.stop />
        <div
          v-for="option in filtered" :key="String(option.value)"
          class="u-select-option" :class="{ 'is-active': option.value === model }"
          @click="pick(option)"
        >{{ option.label }}</div>
        <div v-if="!filtered.length" class="u-select-empty">无匹配项</div>
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.caret-clear {
  position: absolute; right: 26px; top: 50%; transform: translateY(-50%);
  width: 17px; height: 17px; display: grid; place-items: center;
  border-radius: 50%; color: var(--text-3); font-size: 13px; line-height: 1;
  pointer-events: auto;
}
.caret-clear:hover { color: var(--danger); background: var(--canvas-2); }
html.theme-dark .caret-clear:hover { background: var(--border); }
</style>
