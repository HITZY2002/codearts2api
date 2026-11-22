// 控制台共享状态：一次 loadAll 拉取看板/日志/事件，逻辑自旧 App.vue 原样迁移。
import { computed, ref } from 'vue'
import { apiKey, loading, loggedIn, request, setKey } from './api'

// 透传 api.js，页面统一从 store 导入。
export { apiKey, loading, loggedIn, request, setKey }

export const overview = ref(null)
export const metrics = ref({ total_requests: 0, success_rate: 0, ttft_p50_ms: 0, ttft_p95_ms: 0, ttft_p99_ms: 0, total_tokens: 0, error_count: 0 })
export const series = ref([])
export const logs = ref([])
export const events = ref([])

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

export function filterOptions(key, rows = logs.value) {
  return [...new Set(rows.map(row => row[key]).filter(Boolean))].map(value => ({ label: String(value), value: String(value) }))
}

let loadSeq = 0

export async function loadAll() {
  const seq = ++loadSeq
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
      request(`/admin/api/logs?${query}&${filterQuery}&limit=${logPageSize.value}&offset=${(logPage.value - 1) * logPageSize.value}`),
      request(`/admin/api/events?${query}&${eventFilterQuery}&limit=${eventPageSize.value}&offset=${(eventPage.value - 1) * eventPageSize.value}`)
    ])
    if (seq !== loadSeq) return
    overview.value = overviewResult || null
    metrics.value = metricsResult?.metrics || { total_requests: 0, success_rate: 0, ttft_p50_ms: 0, ttft_p95_ms: 0, ttft_p99_ms: 0, total_tokens: 0, error_count: 0 }
    series.value = seriesResult?.items || []
    logs.value = logsResult?.items || []
    logTotal.value = logsResult?.total || 0
    events.value = eventsResult?.items || []
    eventTotal.value = eventsResult?.total || 0
    loggedIn.value = true
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
  setKey('')
  loggedIn.value = false
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
  await request(path, { method: 'POST', body: JSON.stringify(body) })
  await loadAll()
}
