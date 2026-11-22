<script setup>
defineProps({
  options: { type: Array, required: true }, // [{ label, value }]
  size: { type: String, default: 'md' } // md | sm
})
const model = defineModel()
</script>

<template>
  <div class="seg-group" :class="{ 'seg-group--sm': size === 'sm' }">
    <button
      v-for="option in options" :key="option.value" type="button"
      class="seg-item" :class="{ 'is-active': model === option.value }"
      @click="model = option.value"
    >{{ option.label }}</button>
  </div>
</template>
