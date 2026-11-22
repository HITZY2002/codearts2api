<script setup>
import UiSpinner from './UiSpinner.vue'

defineProps({
  variant: { type: String, default: 'soft' }, // primary | outline | soft | ghost | danger
  size: { type: String, default: 'md' }, // md | sm | xs
  loading: Boolean,
  disabled: Boolean,
  type: { type: String, default: 'button' }
})
</script>

<template>
  <button
    :type="type" class="u-btn"
    :class="[`u-btn--${variant}`, size !== 'md' && `u-btn--${size}`, { 'is-disabled': disabled || loading }]"
    :disabled="disabled || loading"
  >
    <UiSpinner v-if="loading" :size="13" class="spin" />
    <slot />
  </button>
</template>
