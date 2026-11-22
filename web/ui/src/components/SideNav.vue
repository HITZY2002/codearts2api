<script setup>
import Icon from './ui/Icon.vue'

defineProps({
  groups: { type: Array, required: true },
  active: { type: String, required: true },
  collapsed: Boolean,
  floating: Boolean
})

const emit = defineEmits(['select'])
</script>

<template>
  <nav class="sidebar" :class="{ collapsed, 'drawer-side': floating }">
    <div class="brand">
      <span class="logo">CA</span>
      <span class="brand-name">
        <strong>CodeArts2API</strong>
        <span>运行控制台</span>
      </span>
    </div>

    <template v-for="group in groups" :key="group.label">
      <div v-if="!collapsed" class="nav-group-label">{{ group.label }}</div>
      <button
        v-for="item in group.items" :key="item.key"
        class="nav-item" :class="{ 'is-active': active === item.key }"
        :title="collapsed ? item.label : undefined"
        @click="emit('select', item.key)"
      >
        <Icon :name="item.icon" :size="16" />
        <span v-if="!collapsed" class="label">{{ item.label }}</span>
      </button>
    </template>
  </nav>
</template>

<style scoped>
.sidebar.collapsed .nav-item { justify-content: center; padding: 9px; }
.sidebar.collapsed .nav-group-label { display: none; }
</style>
