<script setup>
import { computed } from 'vue'

const props = defineProps({
  min: { type: Number, default: 0 },
  max: { type: Number, default: 2147483647 },
  disabled: Boolean,
  placeholder: String
})

const model = defineModel()

const display = computed(() => (model.value == null ? '' : String(model.value)))

function commit(event) {
  const raw = event.target.value.trim()
  const fallback = typeof model.value === 'number' && !Number.isNaN(model.value) ? model.value : props.min
  let value = raw === '' ? fallback : Number(raw)
  if (Number.isNaN(value)) value = fallback
  value = Math.min(props.max, Math.max(props.min, Math.round(value)))
  event.target.value = String(value)
  model.value = value
}
</script>

<template>
  <span class="u-input-number">
    <input
      type="number" :value="display" :min="min" :max="max" :disabled="disabled" :placeholder="placeholder"
      @change="commit"
    />
  </span>
</template>
