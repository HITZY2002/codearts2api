<script setup>
import { ref } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  type: { type: String, default: 'text' },
  placeholder: String,
  size: { type: String, default: 'md' }, // md | sm
  disabled: Boolean,
  clearable: Boolean,
  showPassword: Boolean // type=password 时按住眼睛显示明文
})

const model = defineModel()
const reveal = ref(false)

function onInput(event) {
  model.value = event.target.value
}
function actualType() {
  if (props.type === 'password') return reveal.value ? 'text' : 'password'
  return props.type
}
function clear() {
  model.value = ''
}
</script>

<template>
  <span class="u-input-wrap">
    <input
      class="u-input" :class="{ 'u-input--sm': size === 'sm' }"
      :type="actualType()" :value="model == null ? '' : model" :placeholder="placeholder" :disabled="disabled"
      @input="onInput"
    />
    <span v-if="clearable && model" class="u-input-trail">
      <button class="input-trail-btn" type="button" tabindex="-1" title="清空" @click="clear">
        <Icon name="x" :size="14" />
      </button>
    </span>
    <span v-if="showPassword" class="u-input-trail">
      <button class="input-trail-btn" type="button" tabindex="-1" title="按住查看明文" @mousedown.prevent="reveal = !reveal">
        <Icon :name="reveal ? 'eye-off' : 'eye'" :size="15" />
      </button>
    </span>
  </span>
</template>
