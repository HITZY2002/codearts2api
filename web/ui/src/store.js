// 控制台共享状态：一次 loadAll 拉取看板/日志/事件，逻辑自旧 App.vue 原样迁移。
import { computed, ref } from 'vue'
import { apiKey, loading, loggedIn, request, setKey } from './api'

// 透传 api.js，页面统一从 store 导入。
export { apiKey, loading, loggedIn, request, setKey }

function defaultMetrics() {
  return { total_requests: 0, success_rate: 0, ttft_p50_ms: 0, ttft_p95_ms: 0, ttft_p99_ms: 0, total_tokens: 0, error_count: 0 }
}

export const overview = ref(null)
export const metrics = ref(defaultMetrics())
export const series = ref([])
export const logs = ref([])
export const dashboardLogs = ref([])
export const events = ref([])
export const observabilityDisabled = ref(false)
export const actionLoading = ref('')
export const actionFeedback = ref(null) // { type: 'success' | 'error' | 'warning', message: string }

export const range = ref(24)
export const rangeOptions = [
  { label: '近 1 小时', value: 1 },
  { label: '近 24 小时', value: 24 },
  { label: '近 7 天', value: 168 },
  { label: '近 30 天', value: 720 }
]

export const logFilters = ref({ status_code: null, model: null, account_uid: null, error_type: null, stream: null })
export const eventFilters = ref({ level: null, kind: null, account_uid: null, model: null })
export const logTotal = ref(0)
export const logPage = ref(1)
export const logPageSize = ref(50)
export const eventTotal = ref(0)
export const eventPage = ref(1)
export const eventPageSize = ref(50)

// 详情抽屉（日志 / 事件共用）
export const selectedLog = ref(null)
export const detailOpen = ref(false)

export const accountRows = computed(() => overview.value?.accounts || [])

const seenOptionValues = ref({
  logs: { model: new Set(), account_uid: new Set(), error_type: new Set() },
  events: { kind: new Set(), account_uid: new Set(), model: new Set() }
})

function collectSeenOptions(bucket, rows, keys) {
  const store = seenOptionValues.value[bucket]
  if (!store) return
  for (const row of rows || []) {
    for (const key of keys) {
      if (row?.[key]) store[key]?.add(String(row[key]))
    }
  }
}

export function filterOptions(key, rows = logs.value, currentValue = null) {
  const bucket = rows === events.value ? 'events' : 'logs'
  const values = new Set()
  const remembered = seenOptionValues.value[bucket]?.[key]
  if (remembered) {
    for (const v of remembered) values.add(v)
  }
  if (key === 'model') {
    for (const m of overview.value?.models || []) {
      if (m?.id) values.add(String(m.id))
    }
  }
  if (key === 'account_uid') {
    for (const a of overview.value?.accounts || []) {
      if (a?.uid) values.add(String(a.uid))
    }
  }
  for (const row of rows || []) {
    if (row?.[key]) values.add(String(row[key]))
  }
  if (currentValue !== null && currentValue !== undefined && currentValue !== '') {
    values.add(String(currentValue))
  }
  return [...values].sort().map(value => ({ label: value, value }))
}

function seriesIntervalForRange(hours) {
  if (hours <= 1) return '5m'
  if (hours <= 24) return 'hour'
  return 'day'
}

let loadSeq = 0

export async function loadAll() {
  const seq = ++loadSeq
  loading.value = true
  try {
    const overviewResult = await request('/admin/api/overview')
    if (seq !== loadSeq) return
    overview.value = overviewResult || null
    loggedIn.value = true

    const now = Date.now()
    const from = now - range.value * 3600000
    const query = `from=${from}&to=${now}`
    const interval = seriesIntervalForRange(range.value)
    const activeLogFilterEntries = Object.entries(logFilters.value).filter(([, value]) => value !== null && value !== '')
    const filterQuery = new URLSearchParams(activeLogFilterEntries.map(([key, value]) => [key, String(value)]))
    const eventFilterQuery = new URLSearchParams(Object.entries(eventFilters.value).filter(([, value]) => value !== null && value !== '').map(([key, value]) => [key, String(value)]))
    const needSeparateDashboardLogs = activeLogFilterEntries.length > 0 || logPage.value > 1

    const requests = [
      request(`/admin/api/metrics/overview?${query}`),
      request(`/admin/api/metrics/series?${query}&interval=${interval}`),
      request(`/admin/api/logs?${query}&${filterQuery}&limit=${logPageSize.value}&offset=${(logPage.value - 1) * logPageSize.value}`),
      request(`/admin/api/events?${query}&${eventFilterQuery}&limit=${eventPageSize.value}&offset=${(eventPage.value - 1) * eventPageSize.value}`)
    ]
    if (needSeparateDashboardLogs) {
      requests.push(request(`/admin/api/logs?${query}&limit=200&offset=0`))
    }
    const results = await Promise.allSettled(requests)
    if (seq !== loadSeq) return

    const [metricsRes, seriesRes, logsRes, eventsRes, dashLogsRes] = results
    const obsResults = results
    const disabled = obsResults.some(res =>
      res.status === 'rejected' &&
      (res.reason?.status === 503 || String(res.reason?.message || '').includes('observability disabled'))
    )
    observabilityDisabled.value = disabled
    if (disabled) {
      metrics.value = defaultMetrics()
      series.value = []
      logs.value = []
      dashboardLogs.value = []
      logTotal.value = 0
      events.value = []
      eventTotal.value = 0
    } else {
      let needPageClampReload = false
      if (metricsRes.status === 'fulfilled') {
        metrics.value = metricsRes.value?.metrics || defaultMetrics()
      }
      if (seriesRes.status === 'fulfilled') {
        series.value = seriesRes.value?.items || []
      }
      if (logsRes.status === 'fulfilled') {
        logs.value = logsRes.value?.items || []
        logTotal.value = logsRes.value?.total || 0
        collectSeenOptions('logs', logs.value, ['model', 'account_uid', 'error_type'])
        const maxLogPage = Math.max(1, Math.ceil(logTotal.value / logPageSize.value))
        if (logPage.value > maxLogPage) {
          logPage.value = maxLogPage
          needPageClampReload = true
        }
      }
      if (needSeparateDashboardLogs) {
        if (dashLogsRes?.status === 'fulfilled') {
          dashboardLogs.value = dashLogsRes.value?.items || []
          collectSeenOptions('logs', dashboardLogs.value, ['model', 'account_uid', 'error_type'])
        }
      } else if (logsRes.status === 'fulfilled') {
        dashboardLogs.value = logs.value
      }
      if (eventsRes.status === 'fulfilled') {
        events.value = eventsRes.value?.items || []
        eventTotal.value = eventsRes.value?.total || 0
        collectSeenOptions('events', events.value, ['kind', 'account_uid', 'model'])
        const maxEventPage = Math.max(1, Math.ceil(eventTotal.value / eventPageSize.value))
        if (eventPage.value > maxEventPage) {
          eventPage.value = maxEventPage
          needPageClampReload = true
        }
      }
      for (const res of obsResults) {
        if (res.status === 'rejected') window.console.warn(res.reason)
      }
      if (needPageClampReload) {
        return loadAll()
      }
    }
  } catch (error) {
    if (seq !== loadSeq) return
    if (error?.status === 401 || error?.status === 403) {
      // 真正的鉴权失败才清除持久化的 key。
      loggedIn.value = false
      setKey('')
      loginError.value = 'API Key 无效或已变更，请重新登录'
    } else if (!loggedIn.value) {
      // 进入时的网络/服务抖动：保留 key，用户回到登录页可直接重试。
      loginError.value = apiKey.value ? `服务暂时不可用：${error.message}` : ''
    } else {
      // 已登录状态下的偶发失败：保持界面与现有数据，不强制登出。
      window.console.warn(error)
    }
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

export const loginError = ref('')

export async function login() {
  setKey(apiKey.value)
  loginError.value = ''
  await loadAll()
  if (!loggedIn.value && !loginError.value) loginError.value = 'API Key 无效或服务不可达'
}

export function logout() {
  loadSeq++
  loading.value = false
  setKey('')
  loggedIn.value = false
  loginError.value = ''
  observabilityDisabled.value = false
  overview.value = null
  metrics.value = defaultMetrics()
  series.value = []
  logs.value = []
  dashboardLogs.value = []
  logTotal.value = 0
  events.value = []
  eventTotal.value = 0
  selectedLog.value = null
  detailOpen.value = false
  actionLoading.value = ''
  actionFeedback.value = null
  seenOptionValues.value = {
    logs: { model: new Set(), account_uid: new Set(), error_type: new Set() },
    events: { kind: new Set(), account_uid: new Set(), model: new Set() }
  }
}

export function reloadDashboard() {
  logPage.value = 1
  eventPage.value = 1
  loadAll()
}

export function reloadLogs() {
  logPage.value = 1
  loadAll()
}

export function reloadEvents() {
  eventPage.value = 1
  loadAll()
}

export function changeLogPage(page) {
  logPage.value = page
  loadAll()
}

export function changeLogPageSize(size) {
  logPageSize.value = size
  logPage.value = 1
  loadAll()
}

export function changeEventPage(page) {
  eventPage.value = page
  loadAll()
}

export function changeEventPageSize(size) {
  eventPageSize.value = size
  eventPage.value = 1
  loadAll()
}

export async function runAction(path, body = {}) {
  if (actionLoading.value) return
  actionLoading.value = path
  actionFeedback.value = null
  try {
    const result = await request(path, { method: 'POST', body: JSON.stringify(body) })
    actionFeedback.value = {
      type: result?.ok === false ? 'warning' : 'success',
      message: result?.message || '操作已完成'
    }
    await loadAll()
  } catch (error) {
    actionFeedback.value = {
      type: 'error',
      message: `操作失败：${error.message}`
    }
  } finally {
    actionLoading.value = ''
  }
}
