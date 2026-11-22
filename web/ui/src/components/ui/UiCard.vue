<script setup>
defineProps({ title: String, flush: Boolean })
</script>

<template>
  <section class="u-card">
    <header v-if="title || $slots['header-extra']" class="u-card-header">
      <h3>{{ title }}</h3>
      <div v-if="$slots['header-extra']" class="u-card-header-extra"><slot name="header-extra" /></div>
    </header>
    <div class="u-card-body" :class="{ flush }"><slot /></div>
  </section>
</template>
