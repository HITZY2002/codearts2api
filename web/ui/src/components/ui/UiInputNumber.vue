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
  let value = raw === '' ? null : Number(raw)
  if (value != null && Number.isNaN(value)) value = model.value
  if (value != null) value = Math.min(props.max, Math.max(props.min, Math.round(value)))
  event.target.value = value == null ? '' : String(value)
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
