// 格式化与文案映射（自旧 App.vue 原样迁移）。

export function formatTime(value) {
  if (!value) return '—'
  return new Date(value).toLocaleString()
}

export function formatNumber(value) {
  return Number(value || 0).toLocaleString()
}

export function formatCredit(value) {
  return Number(value || 0).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
}

export function formatDuration(value) {
  if (!value) return '0 ms'
  return value >= 1000 ? `${(value / 1000).toFixed(1)} s` : `${formatNumber(value)} ms`
}

// token_remaining 是 Go duration 字符串（如 "119h59m0s"，Token 有效期剩余），转人话。
export function formatRemaining(value) {
  if (!value) return '—'
  const m = /^((\d+)h)?((\d+)m)?((\d+(?:\.\d+)?)s)?$/.exec(String(value))
  if (!m) return String(value)
  const hours = Number(m[2] || 0)
  const minutes = Number(m[4] || 0)
  const seconds = Math.round(Number(m[6] || 0))
  const days = Math.floor(hours / 24)
  const parts = []
  if (days) parts.push(`${days}天`)
  if (hours % 24) parts.push(`${hours % 24}时`)
  if (minutes) parts.push(`${minutes}分`)
  if (!days && seconds) parts.push(`${seconds}秒`)
  return parts.join('') || '0秒'
}

// 图表调色板（与 TrendChart 保持一致）。
export const CHART_PALETTE = ['#2dd4bf', '#8b5cf6', '#f59e0b', '#3b82f6', '#ec4899', '#22c55e']

export function statusType(code) {
  if (code >= 500) return 'error'
  if (code >= 400) return 'warning'
  return 'success'
}

export const eventKindLabels = {
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

export function eventKindLabel(kind) {
  return eventKindLabels[kind] || kind || '—'
}

export function eventLevelLabel(level) {
  return { info: '信息', warn: '警告', error: '错误' }[level] || level || '—'
}

export const campaignStatusLabels = {
  CONFIRMED: '已完成',
  CONSUMED: '已完成',
  CLAIMED: '待确认',
  ELIGIBLE: '可领取'
}

export function campaignStatusLabel(status) {
  return campaignStatusLabels[status] || '未参与'
}

export function campaignStatusType(status) {
  if (status === 'CONFIRMED' || status === 'CONSUMED') return 'success'
  if (status === 'CLAIMED') return 'warning'
  if (status === 'ELIGIBLE') return 'info'
  return 'default'
}

// 延迟竖条颜色：按耗时分档（与 tag 色阶一致）。
export function latencyTone(value) {
  if (value == null) return 'var(--border-strong)'
  if (value >= 10000) return 'var(--danger)'
  if (value >= 3000) return 'var(--warning)'
  return 'var(--success)'
}
