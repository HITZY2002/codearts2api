<script setup>
import { computed, h, onBeforeUnmount, onMounted, ref, watch, watchEffect } from 'vue'
import { NAlert, NButton, NCard, NConfigProvider, NDataTable, NDescriptions, NDescriptionsItem, NDrawer, NDrawerContent, NGrid, NGi, NInput, NLayout, NLayoutContent, NLayoutHeader, NSelect, NSpace, NStatistic, NTabPane, NTabs, NTag, darkTheme } from 'naive-ui'
import { use } from 'echarts/core'
import { BarChart, LineChart } from 'echarts/charts'
import { CanvasRenderer } from 'echarts/renderers'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import VChart from 'vue-echarts'
import { ExternalLink, LogOut, RefreshCw, UserPlus } from 'lucide-vue-next'

use([CanvasRenderer, LineChart, BarChart, GridComponent, LegendComponent, TooltipComponent])

const themeChoice = ref(localStorage.getItem('codearts2api_theme') || 'system')
const systemDark = ref(window.matchMedia('(prefers-color-scheme: dark)').matches)
const darkQuery = window.matchMedia('(prefers-color-scheme: dark)')
const isDark = computed(() => themeChoice.value === 'dark' || (themeChoice.value === 'system' && systemDark.value))
const naiveTheme = computed(() => isDark.value ? darkTheme : null)
const themeOptions = [
  { label: '亮色', value: 'light' },
  { label: '暗色', value: 'dark' },
  { label: '跟随系统', value: 'system' }
]

const onSystemThemeChange = (event) => { systemDark.value = event.matches }
watchEffect(() => {
  document.documentElement.classList.toggle('theme-dark', isDark.value)
  document.documentElement.classList.toggle('theme-light', !isDark.value)
})
onMounted(() => darkQuery.addEventListener('change', onSystemThemeChange))
onBeforeUnmount(() => darkQuery.removeEventListener('change', onSystemThemeChange))
const setTheme = (value) => {
  themeChoice.value = value
  localStorage.setItem('codearts2api_theme', value)
}

const apiKey = ref(localStorage.getItem('codearts2api_panel_api_key') || '')
const loggedIn = ref(false)
const loading = ref(false)
const overview = ref(null)
const metrics = ref({ total_requests: 0, success_rate: 0, ttft_p50_ms: 0, ttft_p95_ms: 0, ttft_p99_ms: 0, total_tokens: 0, error_count: 0 })
const series = ref([])
const logs = ref([])
const events = ref([])
const logFilters = ref({ status_code: null, model: null, account_uid: null, error_type: null, stream: null })
const eventFilters = ref({ level: null, kind: null, account_uid: null, model: null })
const selectedLog = ref(null)
const detailOpen = ref(false)
const logTotal = ref(0)
const logPage = ref(1)
const eventTotal = ref(0)
const eventPage = ref(1)
const range = ref(24)
const pageSize = 50
const rangeOptions = [
  { label: '近 1 小时', value: 1 },
  { label: '近 24 小时', value: 24 },
  { label: '近 7 天', value: 168 },
  { label: '近 30 天', value: 720 }
]
const page = ref('overview')
const oauthLoading = ref(false)
const oauthSession = ref(null)
const oauthStatus = ref('')
const oauthError = ref(false)
const oauthCallbackURL = ref('')

const accountRows = computed(() => overview.value?.accounts || [])
const accountSearch = ref('')
const creditBalances = ref({})
const creditTotal = ref({ total_amount: 0, used_amount: 0, remaining_amount: 0, expiring_amount: 0 })
const creditLoaded = ref(false)
const creditLoading = ref(false)
const creditError = ref('')
const accountActionLoading = ref({})
const accountActionFeedback = ref({})
const batchCheckinOpen = ref(false)
const batchCheckinLoading = ref(false)
const batchCheckinSize = ref(3)
const batchCheckinResults = ref([])
const batchCheckinMessage = ref('')
const batchCheckinError = ref(false)
const filteredAccountRows = computed(() => {
  const keyword = accountSearch.value.trim().toLowerCase()
  return accountRows.value
    .filter(account => !keyword || [account.nickname, account.name, account.uid]
      .filter(Boolean)
      .some(value => String(value).toLowerCase().includes(keyword)))
    .map(account => ({ ...account, credit_balance: creditBalances.value[account.uid] || null }))
})
const modelRows = computed(() => overview.value?.models || [])

const requestOption = computed(() => ({
  tooltip: { trigger: 'axis' },
  backgroundColor: 'transparent', textStyle: { color: isDark.value ? '#cbd5e1' : '#475569' },
  legend: { data: ['请求', '成功', '失败', 'Token'], textStyle: { color: isDark.value ? '#cbd5e1' : '#475569' } },
  grid: { left: 42, right: 48, top: 42, bottom: 28 },
  xAxis: { type: 'category', data: series.value.map(item => new Date(item.bucket).toLocaleTimeString()) },
  yAxis: [
    { type: 'value', name: '次数' },
    { type: 'value', name: 'Token', splitLine: { show: false } }
  ],
  series: [
    { name: '请求', type: 'line', smooth: true, showSymbol: false, data: series.value.map(item => item.request_count) },
    { name: '成功', type: 'line', smooth: true, showSymbol: false, data: series.value.map(item => item.success_count) },
    { name: '失败', type: 'bar', data: series.value.map(item => item.error_count) },
    { name: 'Token', type: 'line', yAxisIndex: 1, smooth: true, showSymbol: false, data: series.value.map(item => item.total_tokens) }
  ]
}))

const latencyOption = computed(() => ({
  tooltip: { trigger: 'axis' },
  backgroundColor: 'transparent', textStyle: { color: isDark.value ? '#cbd5e1' : '#475569' },
  legend: { data: ['平均 TTFT', '平均耗时'], textStyle: { color: isDark.value ? '#cbd5e1' : '#475569' } },
  grid: { left: 42, right: 24, top: 42, bottom: 28 },
  xAxis: { type: 'category', data: series.value.map(item => new Date(item.bucket).toLocaleTimeString()) },
  yAxis: { type: 'value', name: 'ms' },
  series: [
    { name: '平均 TTFT', type: 'line', smooth: true, showSymbol: false, data: series.value.map(item => item.avg_ttft_ms) },
    { name: '平均耗时', type: 'line', smooth: true, showSymbol: false, data: series.value.map(item => item.avg_total_ms) }
  ]
}))

const logColumns = [
  { title: '时间', key: 'created_at', width: 165, render: row => formatTime(row.created_at) },
  { title: '模型', key: 'model', width: 150 },
  { title: '账号', key: 'account_name', width: 150, ellipsis: { tooltip: true } },
  { title: '状态', key: 'status_code', width: 86, render: row => h(NTag, { size: 'small', bordered: false, type: statusType(row.status_code) }, { default: () => String(row.status_code) }) },
  { title: 'TTFT', key: 'first_output_ms', width: 105, render: row => row.first_output_ms == null ? '—' : `${formatNumber(row.first_output_ms)} ms` },
  { title: '耗时', key: 'total_ms', width: 105, render: row => `${formatNumber(row.total_ms)} ms` },
  { title: 'Token', key: 'total_tokens', width: 105, render: row => formatNumber(row.total_tokens) },
  { title: '积分', key: 'credit_cost', width: 90, render: row => row.credit_cost == null ? '—' : formatNumber(row.credit_cost) },
  { title: 'IP', key: 'client_ip', width: 135 },
  { title: '详情', key: 'details', width: 80, render: row => h(NButton, { size: 'tiny', onClick: () => { selectedLog.value = row; detailOpen.value = true } }, { default: () => '查看' }) }
]

const eventColumns = [
  { title: '时间', key: 'created_at', width: 165, render: row => formatTime(row.created_at) },
  { title: '级别', key: 'level', width: 85, render: row => h(NTag, { size: 'small', type: row.level === 'error' ? 'error' : row.level === 'warn' ? 'warning' : 'info' }, { default: () => eventLevelLabel(row.level) }) },
  { title: '类型', key: 'kind', width: 170, render: row => h('span', { class: 'truncate-cell', title: row.kind || '' }, eventKindLabel(row.kind)) },
  { title: '账号', key: 'account_uid', width: 170, ellipsis: { tooltip: true } },
  { title: '消息', key: 'message' },
  { title: '数据', key: 'data', width: 80, render: row => h(NButton, { size: 'tiny', onClick: () => { selectedLog.value = { ...row, request_id: `事件 #${row.id}`, event: true }; detailOpen.value = true } }, { default: () => '查看' }) }
]

const accountColumns = [
  {
    title: '账号',
    key: 'nickname',
    width: 250,
    render: row => h('div', { class: 'account-cell' }, [
      h('strong', {}, row.nickname || '未命名'),
      h('span', { class: 'mono muted' }, row.uid || row.name || '—')
    ])
  },
  {
    title: '状态',
    key: 'status',
    width: 80,
    render: row => h(NTag, {
      size: 'small',
      bordered: false,
      type: row.disabled ? 'error' : row.cooling ? 'warning' : 'success',
      title: row.reason || undefined
    }, { default: () => row.disabled ? '禁用' : row.cooling ? '冷却' : '健康' })
  },
  { title: '并发', key: 'active_concurrent', width: 78, render: row => `${row.active_concurrent}/${row.max_concurrent}` },
  { title: '错误', key: 'err_count', width: 70 },
  {
    title: '剩余积分',
    key: 'credit_balance',
    width: 115,
    render: row => {
      if (!row.credit_balance) return '—'
      if (!row.credit_balance.ok) {
        return h(NTag, { size: 'small', type: 'error', title: row.credit_balance.message }, { default: () => '查询失败' })
      }
      return h('strong', {
        class: 'credit-value',
        title: `总额 ${formatCredit(row.credit_balance.total_amount)}，已用 ${formatCredit(row.credit_balance.used_amount)}`
      }, formatCredit(row.credit_balance.remaining_amount))
    }
  },
  { title: 'Token 剩余', key: 'token_remaining', width: 105 },
  { title: '到期时间', key: 'expires_at', width: 155, render: row => formatTime(row.expires_at) },
  {
    title: '最近签到',
    key: 'checkin',
    width: 110,
    render: row => row.checkin
      ? h(NTag, { size: 'small', type: row.checkin.ok ? 'success' : 'error', title: row.checkin.message }, { default: () => formatTime(row.checkin.checked_at) })
      : '—'
  },
  {
    title: '操作',
    key: 'actions',
    width: 200,
    fixed: 'right',
    render: row => {
      const loadingAction = accountActionLoading.value[row.uid]
      const feedback = accountActionFeedback.value[row.uid]
      return h('div', { class: 'account-actions' }, [
        h(NSpace, { size: 6, wrap: false }, {
          default: () => [
            h(NButton, {
              size: 'tiny', secondary: true,
              loading: loadingAction === 'validate',
              disabled: Boolean(loadingAction) && loadingAction !== 'validate',
              onClick: () => runAccountAction(row, 'validate')
            }, { default: () => '校验' }),
            h(NButton, {
              size: 'tiny', secondary: true,
              loading: loadingAction === 'checkin',
              disabled: Boolean(loadingAction) && loadingAction !== 'checkin',
              onClick: () => runAccountAction(row, 'checkin')
            }, { default: () => '签到' }),
            h(NButton, {
              size: 'tiny', secondary: true,
              loading: loadingAction === 'keepalive',
              disabled: Boolean(loadingAction) && loadingAction !== 'keepalive',
              onClick: () => runAccountAction(row, 'keepalive')
            }, { default: () => '保活' })
          ]
        }),
        feedback
          ? h('span', {
              class: ['account-action-feedback', feedback.type],
              title: feedback.message
            }, feedback.message)
          : null
      ])
    }
  }
]

const campaignStatusLabels = {
  CONFIRMED: '已完成',
  CONSUMED: '已完成',
  CLAIMED: '待确认',
  ELIGIBLE: '可领取'
}

function campaignStatusLabel(status) {
  return campaignStatusLabels[status] || '未参与'
}

function campaignStatusType(status) {
  if (status === 'CONFIRMED' || status === 'CONSUMED') return 'success'
  if (status === 'CLAIMED') return 'warning'
  if (status === 'ELIGIBLE') return 'info'
  return 'default'
}

function accountDisplayName(uid) {
  return accountRows.value.find(account => account.uid === uid)?.nickname || uid
}

const batchCheckinColumns = [
  {
    title: '账号',
    key: 'uid',
    width: 230,
    ellipsis: { tooltip: true },
    render: row => accountDisplayName(row.uid)
  },
  {
    title: '结果',
    key: 'result',
    width: 100,
    render: row => {
      const pending = row.campaigns?.some(campaign => campaign.status === 'ELIGIBLE' || campaign.status === 'CLAIMED')
      const label = !row.ok ? '失败' : row.wrote ? '本次已领取' : pending ? '待签到' : '已完成'
      const type = !row.ok ? 'error' : row.wrote ? 'success' : pending ? 'info' : 'default'
      return h(NTag, { size: 'small', type }, { default: () => label })
    }
  },
  { title: '本次积分', key: 'credits', width: 100, render: row => row.credits ? formatCredit(row.credits) : '0.00' },
  {
    title: '活动状态',
    key: 'campaigns',
    render: row => {
      if (row.campaigns?.length) {
        return h('div', { class: 'campaign-list' }, row.campaigns.map(campaign =>
          h('div', { class: 'campaign-row', title: campaign.message || '' }, [
            h('span', { class: 'campaign-title' }, campaign.title || `活动 ${campaign.campaign_id}`),
            h(NTag, { size: 'tiny', type: campaignStatusType(campaign.status) }, {
              default: () => campaignStatusLabel(campaign.status)
            }),
            h('span', { class: 'muted campaign-message' }, campaign.message || '—')
          ])
        ))
      }
      return h('span', { class: 'muted' }, row.message || '无活动')
    }
  }
]

const filterOptions = (key, rows = logs.value) => [...new Set(rows.map(row => row[key]).filter(Boolean))].map(value => ({ label: String(value), value: String(value) }))

function statusType(code) {
  if (code >= 500) return 'error'
  if (code >= 400) return 'warning'
  return 'success'
}

const eventKindLabels = {
  pool_cooldown: '账号冷却',
  pool_note_error: '连续请求错误',
  credit_checkin: '积分签到',
  model_discovery_failed: '模型发现失败',
  model_discovery_empty: '模型列表为空',
  model_discovery_succeeded: '模型发现成功',
  model_unusable: '模型不可用',
  upstream_auth: '上游鉴权失败',
  upstream_rate_limit: '上游限流',
  model_probe_failed: '模型探测失败',
  model_probe_succeeded: '模型探测成功',
  token_refresh_failed: 'Token 刷新失败',
  token_refreshed: 'Token 已刷新'
}

function eventKindLabel(kind) {
  return eventKindLabels[kind] || kind || '—'
}

function eventLevelLabel(level) {
  return { info: '信息', warn: '警告', error: '错误' }[level] || level || '—'
}

function formatTime(value) {
  if (!value) return '—'
  return new Date(value).toLocaleString()
}

function formatNumber(value) {
  return Number(value || 0).toLocaleString()
}

function formatCredit(value) {
  return Number(value || 0).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
}

function authHeaders() {
  return apiKey.value ? { Authorization: `Bearer ${apiKey.value}` } : {}
}

async function request(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...authHeaders(), ...(options.headers || {}) }
  })
  const payload = await response.json().catch(() => ({}))
  if (!response.ok) {
    throw new Error(payload.message || payload.error?.message || `${response.status} ${response.statusText}`)
  }
  return payload
}

async function loadAll() {
  loading.value = true
  try {
    const now = Date.now()
    const from = now - range.value * 3600000
    const query = `from=${from}&to=${now}`
    const filterQuery = new URLSearchParams(Object.entries(logFilters.value).filter(([, value]) => value !== null && value !== '').map(([key, value]) => [key, String(value)]))
    const eventFilterQuery = new URLSearchParams(Object.entries(eventFilters.value).filter(([, value]) => value !== null && value !== '').map(([key, value]) => [key, String(value)]))
    const [overviewResult, metricsResult, seriesResult, logsResult, eventsResult] = await Promise.all([
      request('/admin/api/overview'),
      request(`/admin/api/metrics/overview?${query}`),
      request(`/admin/api/metrics/series?${query}&interval=hour`),
      request(`/admin/api/logs?${query}&${filterQuery}&limit=${pageSize}&offset=${(logPage.value - 1) * pageSize}`),
      request(`/admin/api/events?${query}&${eventFilterQuery}&limit=${pageSize}&offset=${(eventPage.value - 1) * pageSize}`)
    ])
    overview.value = overviewResult
    metrics.value = metricsResult.metrics
    series.value = seriesResult.items
    logs.value = logsResult.items
    logTotal.value = logsResult.total
    events.value = eventsResult.items
    eventTotal.value = eventsResult.total
    loggedIn.value = true
  } catch (error) {
    loggedIn.value = false
    apiKey.value = ''
    localStorage.removeItem('codearts2api_panel_api_key')
    window.console.warn(error)
  } finally {
    loading.value = false
  }
}

async function login() {
  localStorage.setItem('codearts2api_panel_api_key', apiKey.value)
  await loadAll()
}

function reloadDashboard() {
  logPage.value = 1
  eventPage.value = 1
  loadAll()
}

function reloadLogs() {
  logPage.value = 1
  loadAll()
}

function reloadEvents() {
  eventPage.value = 1
  loadAll()
}

function logout() {
  apiKey.value = ''
  localStorage.removeItem('codearts2api_panel_api_key')
  loggedIn.value = false
}

async function runAction(path, body = {}) {
  await request(path, { method: 'POST', body: JSON.stringify(body) })
  await loadAll()
  creditLoaded.value = false
  if (page.value === 'accounts') await loadCreditBalances()
}

async function runAccountAction(account, action) {
  const uid = account.uid
  const actions = {
    validate: { path: '/admin/api/credits', label: '校验' },
    checkin: { path: '/admin/api/checkin', label: '签到' },
    keepalive: { path: '/admin/api/keepalive', label: '保活' }
  }
  const selected = actions[action]
  accountActionLoading.value = { ...accountActionLoading.value, [uid]: action }
  try {
    const result = await request(selected.path, {
      method: 'POST',
      body: JSON.stringify({ uid })
    })
    const item = result.results?.find(entry => entry.uid === uid)
    const ok = item ? item.ok : result.ok
    let message = item?.message || result.message || '操作完成'
    if (ok && action === 'validate' && message === 'ok') message = '登录态有效'
    if (ok && action === 'keepalive' && message === 'refreshed/validated') message = 'Token 有效，已检查续期'
    if (ok && action === 'checkin' && item?.skipped) message = '今日已签到，未重复领取'
    const feedbackType = ok ? (item?.skipped ? 'info' : 'success') : 'error'
    accountActionFeedback.value = {
      ...accountActionFeedback.value,
      [uid]: {
        type: feedbackType,
        message: `${selected.label}${ok ? '完成' : '失败'}：${message}`
      }
    }
    await loadAll()
    creditLoaded.value = false
    if (page.value === 'accounts') await loadCreditBalances()
  } catch (error) {
    accountActionFeedback.value = {
      ...accountActionFeedback.value,
      [uid]: { type: 'error', message: `${selected.label}失败：${error.message}` }
    }
  } finally {
    const next = { ...accountActionLoading.value }
    delete next[uid]
    accountActionLoading.value = next
  }
}

async function runBatchCheckin() {
  batchCheckinLoading.value = true
  batchCheckinMessage.value = ''
  batchCheckinError.value = false
  try {
    const result = await request('/admin/api/checkin', {
      method: 'POST',
      body: JSON.stringify({ batch_size: batchCheckinSize.value })
    })
    batchCheckinResults.value = result.results || []
    batchCheckinMessage.value = result.message || '批量签到完成'
    batchCheckinError.value = !result.ok
    const feedback = { ...accountActionFeedback.value }
    for (const item of batchCheckinResults.value) {
      feedback[item.uid] = {
        type: !item.ok ? 'error' : item.skipped ? 'info' : 'success',
        message: !item.ok ? `签到失败：${item.message || '未知错误'}` : item.skipped ? '签到完成：今日已签到' : `签到成功：领取 ${formatCredit(item.credits)} 积分`
      }
    }
    accountActionFeedback.value = feedback
    await loadAll()
    creditLoaded.value = false
    if (page.value === 'accounts') await loadCreditBalances()
  } catch (error) {
    batchCheckinMessage.value = error.message
    batchCheckinError.value = true
  } finally {
    batchCheckinLoading.value = false
  }
}

async function loadBatchCheckinStatus() {
  batchCheckinLoading.value = true
  batchCheckinMessage.value = ''
  batchCheckinError.value = false
  try {
    const result = await request(`/admin/api/checkin/status?batch_size=${batchCheckinSize.value}`)
    batchCheckinResults.value = result.results || []
    batchCheckinMessage.value = result.message || '活动状态查询完成'
    batchCheckinError.value = !result.ok
  } catch (error) {
    batchCheckinMessage.value = error.message
    batchCheckinError.value = true
  } finally {
    batchCheckinLoading.value = false
  }
}

async function loadCreditBalances() {
  creditLoading.value = true
  creditError.value = ''
  try {
    const result = await request('/admin/api/credits/balance')
    const byUID = {}
    for (const item of result.results || []) byUID[item.uid] = item
    creditBalances.value = byUID
    creditTotal.value = result.total || creditTotal.value
    creditLoaded.value = true
    if (!result.ok) creditError.value = result.message || '部分账号余额查询失败'
  } catch (error) {
    creditError.value = error.message
  } finally {
    creditLoading.value = false
  }
}

function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms))
}

async function pollAccountLogin(sessionID) {
  while (oauthSession.value?.session_id === sessionID) {
    try {
      const result = await request('/admin/api/oauth/poll', {
        method: 'POST',
        body: JSON.stringify({ session_id: sessionID })
      })
      oauthStatus.value = result.message || ''
      oauthError.value = result.status === 'error'
      if (result.status === 'done' || result.status === 'error') {
        oauthSession.value = null
        if (result.status === 'done') {
          oauthCallbackURL.value = ''
          creditLoaded.value = false
          await loadAll()
          if (page.value === 'accounts') await loadCreditBalances()
        }
        return
      }
    } catch (error) {
      oauthStatus.value = error.message
      oauthError.value = true
      oauthSession.value = null
      return
    }
    await sleep(2000)
  }
}

async function startAccountLogin() {
  oauthLoading.value = true
  oauthError.value = false
  oauthStatus.value = ''
  oauthCallbackURL.value = ''
  try {
    const result = await request('/admin/api/oauth/start', {
      method: 'POST',
      body: JSON.stringify({})
    })
    oauthSession.value = result
    oauthStatus.value = result.message || '等待授权'
    window.open(result.auth_url, '_blank', 'noopener')
    pollAccountLogin(result.session_id)
  } catch (error) {
    oauthStatus.value = error.message
    oauthError.value = true
  } finally {
    oauthLoading.value = false
  }
}

async function importAccountCallback() {
  if (!oauthSession.value || !oauthCallbackURL.value) return
  try {
    const result = await request('/admin/api/oauth/import-callback', {
      method: 'POST',
      body: JSON.stringify({
        session_id: oauthSession.value.session_id,
        callback_url: oauthCallbackURL.value
      })
    })
    oauthStatus.value = result.message || ''
    oauthError.value = result.status === 'error'
    if (result.next_url) {
      oauthSession.value = { ...oauthSession.value, auth_url: result.next_url }
    }
    if (result.status === 'done') {
      oauthSession.value = null
      oauthCallbackURL.value = ''
      creditLoaded.value = false
      await loadAll()
      if (page.value === 'accounts') await loadCreditBalances()
    }
  } catch (error) {
    oauthStatus.value = error.message
    oauthError.value = true
  }
}

watch(page, value => {
  if (value === 'accounts' && loggedIn.value && !creditLoaded.value && !creditLoading.value) {
    loadCreditBalances()
  }
})

onMounted(async () => {
  if (apiKey.value) await loadAll()
})
</script>

<template>
  <n-config-provider :theme="naiveTheme" :theme-overrides="{ common: { primaryColor: '#0284c7', primaryColorHover: '#0369a1', primaryColorPressed: '#075985' } }">
    <n-layout class="app-shell">
      <n-layout-header class="app-header" bordered>
        <div class="brand">
          <span class="logo">CA</span>
          <div>
            <strong>CodeArts2API</strong>
            <span class="muted">运行控制台</span>
          </div>
        </div>
        <n-space align="center" :size="10">
          <n-select class="theme-select" :value="themeChoice" :options="themeOptions" size="small" @update:value="setTheme" />
          <n-button size="small" quaternary @click="loadAll">
            <template #icon><RefreshCw :size="15" /></template>
          </n-button>
          <n-button v-if="loggedIn" size="small" quaternary @click="logout">
            <template #icon><LogOut :size="15" /></template>
          </n-button>
        </n-space>
      </n-layout-header>

      <n-layout-content class="app-content">
        <n-card v-if="!loggedIn" class="login-card" :bordered="true">
          <h2>登录控制台</h2>
          <n-space vertical :size="14">
            <n-input v-model:value="apiKey" type="password" show-password-on="mousedown" placeholder="API Key" @keyup.enter="login" />
            <n-button type="primary" block :loading="loading" @click="login">进入控制台</n-button>
          </n-space>
        </n-card>

        <div v-else class="dashboard">
          <n-tabs v-model:value="page" type="line" animated class="page-tabs">
            <n-tab-pane name="overview" tab="大屏">
              <n-space class="toolbar" justify="space-between" align="center">
                <n-select class="range-select" v-model:value="range" :options="rangeOptions" size="small" @update:value="reloadDashboard" />
                <n-space :size="8">
                  <n-button size="small" @click="runAction('/admin/api/checkin')">全员签到</n-button>
                  <n-button size="small" @click="runAction('/admin/api/keepalive')">保活 Token</n-button>
                </n-space>
              </n-space>

              <n-grid :x-gap="14" :y-gap="14" cols="2 s:4 m:6" responsive="screen">
                <n-gi span="2 s:1 m:1"><n-card><n-statistic label="请求" :value="formatNumber(metrics.total_requests)" /></n-card></n-gi>
                <n-gi span="2 s:1 m:1"><n-card><n-statistic label="成功率" :value="`${(metrics.success_rate * 100).toFixed(1)}%`" /></n-card></n-gi>
                <n-gi span="2 s:1 m:1"><n-card><n-statistic label="P50 TTFT" :value="`${metrics.ttft_p50_ms} ms`" /></n-card></n-gi>
                <n-gi span="2 s:1 m:1"><n-card><n-statistic label="P95 TTFT" :value="`${metrics.ttft_p95_ms} ms`" /></n-card></n-gi>
                <n-gi span="2 s:1 m:1"><n-card><n-statistic label="Token" :value="formatNumber(metrics.total_tokens)" /></n-card></n-gi>
                <n-gi span="2 s:1 m:1"><n-card><n-statistic label="错误" :value="formatNumber(metrics.error_count)" /></n-card></n-gi>
              </n-grid>

              <n-grid :x-gap="14" :y-gap="14" cols="1 m:2" responsive="screen">
                <n-gi><n-card title="请求趋势"><v-chart class="chart" :theme="isDark ? 'dark' : null" :option="requestOption" autoresize /></n-card></n-gi>
                <n-gi><n-card title="延迟趋势"><v-chart class="chart" :theme="isDark ? 'dark' : null" :option="latencyOption" autoresize /></n-card></n-gi>
              </n-grid>
            </n-tab-pane>

            <n-tab-pane name="logs" tab="请求日志">
              <n-card title="请求日志" class="data-card">
                <template #header-extra><n-tag size="small">{{ formatNumber(logTotal) }}</n-tag></template>
                <n-space class="log-filters" wrap>
                  <n-select v-model:value="logFilters.status_code" clearable placeholder="状态码" :options="[200, 400, 401, 403, 429, 500].map(value => ({ label: String(value), value }))" @update:value="reloadLogs" />
                  <n-select v-model:value="logFilters.model" clearable filterable placeholder="模型" :options="filterOptions('model')" @update:value="reloadLogs" />
                  <n-select v-model:value="logFilters.account_uid" clearable filterable placeholder="账号" :options="filterOptions('account_uid')" @update:value="reloadLogs" />
                  <n-select v-model:value="logFilters.error_type" clearable placeholder="错误类型" :options="filterOptions('error_type')" @update:value="reloadLogs" />
                  <n-select v-model:value="logFilters.stream" clearable placeholder="流式" :options="[{ label: '流式', value: true }, { label: '非流式', value: false }]" @update:value="reloadLogs" />
                </n-space>
                <n-data-table
                  :columns="logColumns"
                  :data="logs"
                  :bordered="false"
                  :loading="loading"
                  size="small"
                  scroll-x="1260"
                  :pagination="{ pageSize, itemCount: logTotal, page: logPage, onUpdatePage: page => { logPage = page; loadAll() } }"
                  :row-key="row => row.request_id"
                />
              </n-card>
            </n-tab-pane>

            <n-tab-pane name="accounts" tab="账号池">
              <n-card title="账号池" class="data-card">
                <template #header-extra>
                  <n-space align="center" :size="8">
                    <n-tag
                      v-if="creditLoaded"
                      type="success"
                      size="small"
                      :title="`总额 ${formatCredit(creditTotal.total_amount)}，已用 ${formatCredit(creditTotal.used_amount)}`"
                    >
                      总剩余 {{ formatCredit(creditTotal.remaining_amount) }}
                    </n-tag>
                    <n-button size="small" secondary @click="batchCheckinOpen = true">
                      批量签到
                    </n-button>
                    <n-button size="small" secondary :loading="creditLoading" @click="loadCreditBalances">
                      刷新余额
                    </n-button>
                    <n-button
                      v-if="oauthSession?.auth_url"
                      tag="a"
                      :href="oauthSession.auth_url"
                      target="_blank"
                      rel="noopener"
                      secondary
                    >
                      <template #icon><ExternalLink :size="15" /></template>
                      授权页
                    </n-button>
                    <n-button type="primary" :loading="oauthLoading" @click="startAccountLogin">
                      <template #icon><UserPlus :size="15" /></template>
                      添加账号
                    </n-button>
                    <n-tag size="small">{{ formatNumber(accountRows.length) }}</n-tag>
                  </n-space>
                </template>
                <n-space v-if="oauthStatus || oauthSession" vertical :size="12" class="account-login-panel">
                  <n-alert v-if="oauthStatus" :type="oauthError ? 'error' : 'info'" :show-icon="true">
                    {{ oauthStatus }}
                  </n-alert>
                  <n-space v-if="oauthSession" :size="8">
                    <n-input
                      v-model:value="oauthCallbackURL"
                      placeholder="127.0.0.1/oauth/callback 回调地址"
                      @keyup.enter="importAccountCallback"
                    />
                    <n-button
                      v-if="oauthCallbackURL"
                      :loading="oauthLoading"
                      @click="importAccountCallback"
                    >
                      导入回调
                    </n-button>
                  </n-space>
                </n-space>
                <n-alert v-if="creditError" type="warning" :show-icon="true" class="credit-alert">
                  {{ creditError }}
                </n-alert>
                <n-space class="log-filters" wrap>
                  <n-input v-model:value="accountSearch" clearable placeholder="搜索账号或 UID" class="account-search" />
                </n-space>
                <n-data-table
                  :columns="accountColumns"
                  :data="filteredAccountRows"
                  :loading="loading"
                  :bordered="false"
                  size="small"
                  striped
                  scroll-x="1153"
                  :row-key="row => row.uid"
                />
              </n-card>
            </n-tab-pane>

            <n-tab-pane name="events" tab="事件">
              <n-card title="事件" class="data-card">
                <template #header-extra><n-tag size="small">{{ formatNumber(eventTotal) }}</n-tag></template>
                <n-space class="log-filters" wrap>
                  <n-select v-model:value="eventFilters.level" clearable placeholder="级别" :options="['info', 'warn', 'error'].map(value => ({ label: value, value }))" @update:value="reloadEvents" />
                  <n-select v-model:value="eventFilters.kind" clearable filterable placeholder="类型" :options="filterOptions('kind', events)" @update:value="reloadEvents" />
                  <n-select v-model:value="eventFilters.account_uid" clearable filterable placeholder="账号" :options="filterOptions('account_uid', events)" @update:value="reloadEvents" />
                  <n-select v-model:value="eventFilters.model" clearable filterable placeholder="模型" :options="filterOptions('model', events)" @update:value="reloadEvents" />
                </n-space>
                <n-data-table
                  :columns="eventColumns"
                  :data="events"
                  :loading="loading"
                  size="small"
                  :bordered="false"
                  scroll-x="820"
                  :pagination="{ pageSize, itemCount: eventTotal, page: eventPage, onUpdatePage: page => { eventPage = page; loadAll() } }"
                  :row-key="row => row.id"
                />
              </n-card>
            </n-tab-pane>

            <n-tab-pane name="models" tab="模型">
              <n-grid :x-gap="14" :y-gap="14" cols="2 s:3 m:4" responsive="screen">
                <n-gi v-for="model in modelRows" :key="model.id">
                  <n-card>
                    <div class="mono">{{ model.id }}</div>
                  </n-card>
                </n-gi>
              </n-grid>
            </n-tab-pane>
          </n-tabs>
          <n-drawer v-model:show="detailOpen" :width="560" placement="right">
            <n-drawer-content title="记录详情" closable>
              <n-descriptions v-if="selectedLog" bordered :column="1" size="small">
                <n-descriptions-item v-for="(value, key) in selectedLog" :key="key" :label="key">
                  <pre v-if="key === 'data'" class="detail-json">{{ typeof value === 'string' ? value : JSON.stringify(value, null, 2) }}</pre>
                  <span v-else>{{ value == null || value === '' ? '—' : String(value) }}</span>
                </n-descriptions-item>
              </n-descriptions>
            </n-drawer-content>
          </n-drawer>
          <n-drawer v-model:show="batchCheckinOpen" :width="760" placement="right">
            <n-drawer-content title="批量签到" closable>
              <n-space vertical :size="14">
                <n-space justify="space-between" align="center">
                  <n-select
                    v-model:value="batchCheckinSize"
                    :options="[1, 3, 5, 10].map(value => ({ label: `每批 ${value} 个账号`, value }))"
                    style="width: 160px"
                  />
                  <n-space :size="8">
                    <n-button :loading="batchCheckinLoading" @click="loadBatchCheckinStatus">
                      查询状态
                    </n-button>
                    <n-button type="primary" :loading="batchCheckinLoading" @click="runBatchCheckin">
                      开始签到
                    </n-button>
                  </n-space>
                </n-space>
                <n-alert v-if="batchCheckinMessage" :type="batchCheckinError ? 'warning' : 'success'" :show-icon="true">
                  {{ batchCheckinMessage }}
                </n-alert>
                <n-data-table
                  :columns="batchCheckinColumns"
                  :data="batchCheckinResults"
                  :loading="batchCheckinLoading"
                  :bordered="false"
                  size="small"
                  scroll-x="690"
                  :pagination="{ pageSize: 20 }"
                  :row-key="row => row.uid"
                />
              </n-space>
            </n-drawer-content>
          </n-drawer>
        </div>
      </n-layout-content>
    </n-layout>
  </n-config-provider>
</template>

<style>
* { box-sizing: border-box; }
html, body, #app { margin: 0; min-height: 100%; }
html { --app-bg: #f3f6fb; --app-text: #1f2937; --app-muted: #64748b; }
html.theme-dark { --app-bg: #101828; --app-text: #e2e8f0; --app-muted: #94a3b8; }
body {
  color: var(--app-text);
  background: var(--app-bg);
  font-family: Inter, ui-sans-serif, system-ui, -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif;
}
.app-shell { min-height: 100vh; }
.app-header {
  display: flex; align-items: center; justify-content: space-between;
  padding: 14px 18px; gap: 16px;
}
.brand { display: flex; align-items: center; gap: 12px; }
.logo {
  width: 38px; height: 38px; border-radius: 10px;
  display: grid; place-items: center; color: #fff; font-weight: 700;
  background: linear-gradient(135deg, #0284c7, #22d3ee);
}
.brand strong { display: block; font-size: 16px; }
.brand .muted { font-size: 12px; color: var(--app-muted); }
.app-content { padding: 18px; }
.dashboard { max-width: 1440px; margin: 0 auto; display: grid; gap: 16px; }
.dashboard > * { min-width: 0; }
.page-tabs .n-tabs-nav { overflow-x: auto; }
.page-tabs .n-tab-pane { padding-top: 18px; }
.toolbar { align-items: center; }
.range-select { width: 140px; }
.account-search { width: 280px; }
.data-card { border-radius: 8px; }
.data-card .log-filters { margin-bottom: 14px; }
.account-login-panel { margin-bottom: 14px; }
.credit-alert { margin-bottom: 14px; }
.log-filters .n-select { width: 160px; }
.detail-json { white-space: pre-wrap; overflow-wrap: anywhere; margin: 0; }
.theme-select { width: 120px; }
.login-card { max-width: 420px; margin: 12vh auto 0; }
.login-card h2 { margin: 0 0 18px; text-align: center; }
.chart { width: 100%; height: 300px; }
.account-cell { display: grid; gap: 3px; min-width: 0; }
.account-cell strong, .account-cell span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.account-actions { display: grid; gap: 6px; min-width: 0; }
.account-action-feedback { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; line-height: 1.35; }
.account-action-feedback.success { color: #15803d; }
.account-action-feedback.error { color: #dc2626; }
.account-action-feedback.info { color: var(--app-muted); }
html.theme-dark .account-action-feedback.success { color: #4ade80; }
html.theme-dark .account-action-feedback.error { color: #f87171; }
.credit-value { font-variant-numeric: tabular-nums; }
.campaign-list { display: grid; gap: 7px; }
.campaign-row { display: grid; grid-template-columns: minmax(150px, 1fr) auto minmax(140px, 1fr); align-items: center; gap: 8px; }
.campaign-title, .campaign-message { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.truncate-cell { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; }
@media (max-width: 640px) {
  .app-content { padding: 12px; }
  .app-header { flex-wrap: wrap; }
  .theme-select { width: 100%; }
  .range-select, .account-search, .log-filters .n-select { width: 100%; }
  .campaign-row { grid-template-columns: 1fr auto; }
  .campaign-message { grid-column: 1 / -1; }
}
</style>
