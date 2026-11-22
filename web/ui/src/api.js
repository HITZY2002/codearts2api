// 鉴权与 fetch 封装（自旧 App.vue 原样迁移）。
import { ref } from 'vue'

const KEY_STORAGE = 'codearts2api_panel_api_key'

export const apiKey = ref(localStorage.getItem(KEY_STORAGE) || '')
export const loggedIn = ref(false)
export const loading = ref(false)

export function setKey(value) {
  apiKey.value = value
  if (value) localStorage.setItem(KEY_STORAGE, value)
  else localStorage.removeItem(KEY_STORAGE)
}

export function authHeaders() {
  return apiKey.value ? { Authorization: `Bearer ${apiKey.value}` } : {}
}

export async function request(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...authHeaders(), ...(options.headers || {}) }
  })
  const contentType = response.headers.get('content-type') || ''
  let payload = {}
  if (contentType.includes('application/json')) {
    payload = await response.json().catch(() => ({}))
  } else if (response.ok) {
    throw new Error('接口未返回 JSON（请确认后端服务已启动且代理配置正确）')
  }
  if (!response.ok) {
    const error = new Error(payload.message || payload.error?.message || `${response.status} ${response.statusText}`)
    error.status = response.status
    throw error
  }
  return payload
}
