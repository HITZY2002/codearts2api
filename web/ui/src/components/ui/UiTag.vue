<script setup>
defineProps({ type: { type: String, default: 'default' } }) // default | primary | success | warning | error | info
</script>

<template>
  <span class="u-tag" :class="`u-tag--${type}`"><slot /></span>
</template>
