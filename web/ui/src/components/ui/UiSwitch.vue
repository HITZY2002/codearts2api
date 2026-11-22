<script setup>
defineProps({ disabled: Boolean })
const model = defineModel()
</script>

<template>
  <button
    type="button" class="u-switch" :class="{ 'is-on': model }" :disabled="disabled"
    role="switch" :aria-checked="String(model)"
    @click="model = !model"
  />
</template>
