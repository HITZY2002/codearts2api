<script setup>
import { onBeforeUnmount, onMounted } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  title: String,
  width: { type: Number, default: 560 }
})

const show = defineModel({ type: Boolean, default: false })

function close() {
  show.value = false
}
function onKey(event) {
  if (event.key === 'Escape' && show.value) close()
}

onMounted(() => document.addEventListener('keydown', onKey))
onBeforeUnmount(() => document.removeEventListener('keydown', onKey))
</script>

<template>
  <Teleport to="body">
    <Transition name="u-drawer">
      <div v-if="show" class="u-drawer-mask" @click.self="close">
        <div class="u-drawer-panel" :style="{ width: `min(${width}px, 94vw)` }">
          <div class="u-drawer-head">
            <strong>{{ title }}</strong>
            <button class="icon-btn" title="关闭" @click="close">
              <Icon name="x" :size="16" />
            </button>
          </div>
          <div class="u-drawer-body"><slot /></div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
