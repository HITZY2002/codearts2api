<script setup>
import Icon from './Icon.vue'

defineProps({ type: { type: String, default: 'info' } }) // info | success | warning | error

const icons = { info: 'info', success: 'check-circle', warning: 'alert-triangle', error: 'alert-circle' }
</script>

<template>
  <div class="u-alert" :class="`u-alert--${type}`">
    <Icon :name="icons[type] || 'info'" :size="15" />
    <span><slot /></span>
  </div>
</template>
