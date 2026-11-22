<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watchEffect } from 'vue'
import SideNav from './components/SideNav.vue'
import Icon from './components/ui/Icon.vue'
import UiAlert from './components/ui/UiAlert.vue'
import UiButton from './components/ui/UiButton.vue'
import UiInput from './components/ui/UiInput.vue'
import AccountsPage from './pages/AccountsPage.vue'
import DashboardPage from './pages/DashboardPage.vue'
import DetailDrawer from './pages/DetailDrawer.vue'
import EventsPage from './pages/EventsPage.vue'
import LogsPage from './pages/LogsPage.vue'
import ModelsPage from './pages/ModelsPage.vue'
import SettingsPage from './pages/SettingsPage.vue'
import { apiKey, loading, loggedIn } from './api'
import { accountRows, loadAll, login, loginError, logout, overview } from './store'

// ---- 主题（亮 / 暗 / 跟随系统） ----
const themeOrder = ['light', 'dark', 'system']
const themeChoice = ref(localStorage.getItem('codearts2api_theme') || 'system')
const darkQuery = window.matchMedia('(prefers-color-scheme: dark)')
const systemDark = ref(darkQuery.matches)
const isDark = computed(() => themeChoice.value === 'dark' || (themeChoice.value === 'system' && systemDark.value))
const themeIcon = computed(() => ({ light: 'sun', dark: 'moon', system: 'monitor' }[themeChoice.value]))
const themeTitle = computed(() => ({ light: '亮色主题', dark: '暗色主题', system: '跟随系统' }[themeChoice.value]))

const onSystemThemeChange = event => { systemDark.value = event.matches }
watchEffect(() => {
  document.documentElement.classList.toggle('theme-dark', isDark.value)
  document.documentElement.classList.toggle('theme-light', !isDark.value)
})
onMounted(() => {
  darkQuery.addEventListener('change', onSystemThemeChange)
  // 持久化的 API Key 存在时自动登录（loadAll 仅在 401/403 时清除 key）。
  if (apiKey.value) loadAll()
})
onBeforeUnmount(() => darkQuery.removeEventListener('change', onSystemThemeChange))
function cycleTheme() {
  themeChoice.value = themeOrder[(themeOrder.indexOf(themeChoice.value) + 1) % themeOrder.length]
  localStorage.setItem('codearts2api_theme', themeChoice.value)
}

// ---- 导航 ----
const navGroups = [
  {
    label: '监控与观测',
    items: [
      { key: 'dashboard', label: '数据看板', subtitle: '调用趋势、延迟与健康概览', icon: 'dashboard' },
      { key: 'logs', label: '请求日志', subtitle: '网关请求元数据与耗时明细', icon: 'logs' },
      { key: 'events', label: '系统事件', subtitle: '调度、签到与账号状态事件', icon: 'events' },
      { key: 'models', label: '模型列表', subtitle: '/v1/models 可用模型目录', icon: 'package' }
    ]
  },
  {
    label: '池与配置',
    items: [
      { key: 'accounts', label: '账号池', subtitle: '多账号凭证、积分余额与签到', icon: 'accounts' },
      { key: 'settings', label: '运行设置', subtitle: '热更新参数（settings.json）', icon: 'settings' }
    ]
  }
]
const pageMap = {
  dashboard: DashboardPage,
  logs: LogsPage,
  events: EventsPage,
  models: ModelsPage,
  accounts: AccountsPage,
  settings: SettingsPage
}
const page = ref('dashboard')
const currentNavItem = computed(() => {
  for (const group of navGroups) {
    const item = group.items.find(entry => entry.key === page.value)
    if (item) return item
  }
  return null
})
const pageTitle = computed(() => currentNavItem.value?.label || '')
const pageSubtitle = computed(() => currentNavItem.value?.subtitle || '')

const collapsed = ref(false)
const mobileNavOpen = ref(false)
const healthyAccountCount = computed(() =>
  overview.value?.stats?.healthy ?? accountRows.value.filter(a => !a.disabled && !a.cooling).length
)
function selectPage(key) {
  page.value = key
  mobileNavOpen.value = false
}
</script>

<template>
  <!-- 登录页 -->
  <div v-if="!loggedIn" class="login-wrap">
    <div class="login-card">
      <div class="login-head">
        <span class="logo">CA</span>
        <h2>CodeArts2API</h2>
        <p>华为云 CodeArts → OpenAI 兼容网关 · 运行控制台</p>
      </div>
      <form class="login-form" @submit.prevent="login">
        <UiInput v-model="apiKey" type="password" show-password placeholder="API Key" />
        <UiButton variant="primary" type="submit" :loading="loading" style="width: 100%">进入控制台</UiButton>
      </form>
      <UiAlert v-if="loginError" type="error" class="login-error">{{ loginError }}</UiAlert>
    </div>
  </div>

  <!-- 控制台骨架 -->
  <div v-else class="app-frame" :class="{ 'is-collapsed': collapsed }">
    <div class="topbar">
      <div class="topbar-left">
        <button class="icon-btn mobile-only" title="导航" @click="mobileNavOpen = true">
          <Icon name="menu" :size="18" />
        </button>
        <button class="icon-btn desktop-only" :title="collapsed ? '展开侧栏' : '收起侧栏'" @click="collapsed = !collapsed">
          <Icon name="panel-left" :size="17" />
        </button>
        <div class="topbar-heading">
          <span class="topbar-title">{{ pageTitle }}</span>
          <span v-if="pageSubtitle" class="topbar-sub">{{ pageSubtitle }}</span>
        </div>
      </div>
      <div class="topbar-actions">
        <span class="status-pill" :class="{ 'is-warn': accountRows.length && !healthyAccountCount }">
          <span class="pulse" />
          <span>{{ accountRows.length ? `${healthyAccountCount}/${accountRows.length} 账号在线` : '网关在线' }}</span>
        </span>
        <button class="icon-btn" :title="themeTitle" @click="cycleTheme">
          <Icon :name="themeIcon" :size="16" />
        </button>
        <button class="icon-btn" title="刷新数据" :disabled="loading" @click="loadAll()">
          <Icon name="refresh" :size="16" :class="{ spin: loading }" />
        </button>
        <button class="icon-btn" title="退出登录" @click="logout">
          <Icon name="logout" :size="16" />
        </button>
      </div>
    </div>

    <SideNav :groups="navGroups" :active="page" :collapsed="collapsed" @select="selectPage" />

    <!-- 移动端抽屉导航 -->
    <Transition name="fade">
      <div v-if="mobileNavOpen" class="drawer-scrim" @click="mobileNavOpen = false" />
    </Transition>
    <Transition name="side">
      <SideNav v-if="mobileNavOpen" floating :groups="navGroups" :active="page" @select="selectPage" />
    </Transition>

    <main class="app-main">
      <component :is="pageMap[page]" />
    </main>

    <DetailDrawer />
  </div>
</template>

<style scoped>
.desktop-only { display: inline-grid; }
@media (max-width: 860px) {
  .desktop-only { display: none; }
}
.side-enter-active, .side-leave-active { transition: transform 0.22s cubic-bezier(0.33, 1, 0.68, 1), opacity 0.2s ease; }
.side-enter-from, .side-leave-to { transform: translateX(-60px); opacity: 0; }
</style>
