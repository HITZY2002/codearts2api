<script setup>
defineProps({ size: { type: [Number, String], default: 16 } })
</script>

<template>
  <svg class="spinner" :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" aria-hidden="true">
    <path d="M12 3a9 9 0 1 0 9 9" />
  </svg>
</template>
