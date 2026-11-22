<script setup>
import { computed, h, onBeforeUnmount, onMounted, ref } from 'vue'
import UiAlert from '../components/ui/UiAlert.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiCard from '../components/ui/UiCard.vue'
import UiDataTable from '../components/ui/UiDataTable.vue'
import UiDrawer from '../components/ui/UiDrawer.vue'
import UiInput from '../components/ui/UiInput.vue'
import UiSelect from '../components/ui/UiSelect.vue'
import UiTag from '../components/ui/UiTag.vue'
import Icon from '../components/ui/Icon.vue'
import { accountRows, loadAll, loading } from '../store'
import { loggedIn, request } from '../api'
import { campaignStatusLabel, campaignStatusType, formatCredit, formatNumber, formatRemaining, formatTime } from '../utils'

const CALLBACK_HINT = '127.0.0.1/oauth/callback 回调地址'

// ---- 积分余额 ----
const creditBalances = ref({})
const creditTotal = ref({ total_amount: 0, used_amount: 0, remaining_amount: 0, expiring_amount: 0 })
const creditLoaded = ref(false)
const creditLoading = ref(false)
const creditError = ref('')

// ---- 账号操作 ----
const accountSearch = ref('')
const accountActionLoading = ref({})
const accountActionFeedback = ref({})
const pendingDeleteUID = ref('')
let deleteConfirmTimer = null
const reloadLoading = ref(false)
const poolFeedback = ref(null) // { type: 'success' | 'error', message: string }

// ---- OAuth 添加账号 ----
const oauthLoading = ref(false)
const oauthSession = ref(null)
const oauthStatus = ref('')
const oauthError = ref(false)
const oauthCallbackURL = ref('')

// ---- 批量签到 ----
const batchCheckinOpen = ref(false)
const batchCheckinLoading = ref(false)
const batchCheckinSize = ref(3)
const batchCheckinResults = ref([])
const batchCheckinMessage = ref('')
const batchCheckinError = ref(false)
const batchCheckinPage = ref(1)
const batchCheckinPageSize = ref(20)

const batchSizeOptions = [1, 3, 5, 10].map(value => ({ label: `每批 ${value} 个账号`, value }))

// 账号搜索为纯前端过滤，余额按 uid 合并进行数据。
const filteredAccounts = computed(() => {
  const keyword = accountSearch.value.trim().toLowerCase()
  return accountRows.value
    .filter(account => !keyword || [account.nickname, account.name, account.uid]
      .filter(Boolean)
      .some(value => String(value).toLowerCase().includes(keyword)))
    .map(account => ({ ...account, credit_balance: creditBalances.value[account.uid] || null }))
})

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

function accountDisplayName(uid) {
  return accountRows.value.find(account => account.uid === uid)?.nickname || uid
}

async function afterMutation() {
  await loadAll()
  creditLoaded.value = false
  await loadCreditBalances()
}

async function reloadAccountsFromDisk() {
  reloadLoading.value = true
  poolFeedback.value = null
  try {
    const result = await request('/admin/api/reload', { method: 'POST', body: JSON.stringify({}) })
    poolFeedback.value = {
      type: 'success',
      message: `${result.message || '已重载 auths'}（加载 ${result.loaded ?? 0} 个，池内 ${result.total ?? 0} 个，健康 ${result.healthy ?? 0} 个）`
    }
    await afterMutation()
  } catch (error) {
    poolFeedback.value = { type: 'error', message: `重载失败：${error.message}` }
  } finally {
    reloadLoading.value = false
  }
}

async function runAccountAction(account, action) {
  const uid = account.uid
  const actions = {
    validate: { path: '/admin/api/credits', label: '校验' },
    checkin: { path: '/admin/api/checkin', label: '签到' },
    keepalive: { path: '/admin/api/keepalive', label: '保活' },
    enable: { path: '/admin/api/accounts/enable', label: '启用' },
    disable: { path: '/admin/api/accounts/disable', label: '禁用' },
    clearCooldown: { path: '/admin/api/accounts/clear-cooldown', label: '清冷却' }
  }
  const selected = actions[action]
  accountActionLoading.value = { ...accountActionLoading.value, [uid]: action }
  try {
    const result = await request(selected.path, { method: 'POST', body: JSON.stringify({ uid }) })
    const item = result.results?.find(entry => entry.uid === uid)
    const ok = item ? item.ok : result.ok
    let message = item?.message || result.message || '操作完成'
    if (ok && action === 'validate' && message === 'ok') message = '登录态有效'
    if (ok && action === 'keepalive' && message === 'refreshed/validated') message = 'Token 有效，已检查续期'
    if (ok && action === 'checkin' && item?.skipped) message = '今日已签到，未重复领取'
    const feedbackType = ok ? (item?.skipped ? 'info' : 'success') : 'error'
    accountActionFeedback.value = {
      ...accountActionFeedback.value,
      [uid]: { type: feedbackType, message: `${selected.label}${ok ? '完成' : '失败'}：${message}` }
    }
    await afterMutation()
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

function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms))
}

function askDeleteAccount(uid) {
  pendingDeleteUID.value = uid
  clearTimeout(deleteConfirmTimer)
  deleteConfirmTimer = setTimeout(() => {
    if (pendingDeleteUID.value === uid) pendingDeleteUID.value = ''
  }, 6000)
}

function cancelDeleteAccount() {
  pendingDeleteUID.value = ''
  clearTimeout(deleteConfirmTimer)
}

async function confirmDeleteAccount(account) {
  const uid = account.uid
  cancelDeleteAccount()
  accountActionLoading.value = { ...accountActionLoading.value, [uid]: 'delete' }
  poolFeedback.value = null
  try {
    const result = await request('/admin/api/accounts/delete', {
      method: 'POST',
      body: JSON.stringify({ uid })
    })
    poolFeedback.value = {
      type: 'success',
      message: result.message || `已删除账号 ${account.nickname || uid}`
    }
    await afterMutation()
  } catch (error) {
    poolFeedback.value = { type: 'error', message: `删除失败：${error.message}` }
  } finally {
    const next = { ...accountActionLoading.value }
    delete next[uid]
    accountActionLoading.value = next
  }
}

let unmounted = false
onBeforeUnmount(() => {
  unmounted = true
  oauthSession.value = null
  clearTimeout(deleteConfirmTimer)
})

async function pollAccountLogin(sessionID) {
  while (!unmounted && oauthSession.value?.session_id === sessionID) {
    try {
      const result = await request('/admin/api/oauth/poll', {
        method: 'POST',
        body: JSON.stringify({ session_id: sessionID })
      })
      if (unmounted || oauthSession.value?.session_id !== sessionID) return
      oauthStatus.value = result.message || ''
      oauthError.value = result.status === 'error'
      if (result.status === 'done' || result.status === 'error') {
        oauthSession.value = null
        if (result.status === 'done') {
          oauthCallbackURL.value = ''
          await afterMutation()
        }
        return
      }
    } catch (error) {
      if (unmounted || oauthSession.value?.session_id !== sessionID) return
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
    const result = await request('/admin/api/oauth/start', { method: 'POST', body: JSON.stringify({}) })
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
  if (!oauthSession.value || !oauthCallbackURL.value || oauthLoading.value) return
  oauthLoading.value = true
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
      oauthCallbackURL.value = ''
      window.open(result.next_url, '_blank', 'noopener')
    }
    if (result.status === 'done') {
      oauthSession.value = null
      oauthCallbackURL.value = ''
      await afterMutation()
    }
  } catch (error) {
    oauthStatus.value = error.message
    oauthError.value = true
  } finally {
    oauthLoading.value = false
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
    batchCheckinPage.value = 1
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
    await loadCreditBalances()
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
    batchCheckinPage.value = 1
    batchCheckinMessage.value = result.message || '活动状态查询完成'
    batchCheckinError.value = !result.ok
  } catch (error) {
    batchCheckinMessage.value = error.message
    batchCheckinError.value = true
  } finally {
    batchCheckinLoading.value = false
  }
}

const batchCheckinColumns = [
  { title: '账号', key: 'uid', minWidth: 180, render: row => accountDisplayName(row.uid) },
  {
    title: '结果', key: 'result', width: 104,
    render: row => {
      const pending = row.campaigns?.some(campaign => campaign.status === 'ELIGIBLE' || campaign.status === 'CLAIMED')
      const label = !row.ok ? '失败' : row.wrote ? '本次已领取' : pending ? '待签到' : '已完成'
      const type = !row.ok ? 'error' : row.wrote ? 'success' : pending ? 'info' : 'default'
      return h(UiTag, { type }, { default: () => label })
    }
  },
  { title: '本次积分', key: 'credits', width: 100, align: 'right', render: row => h('span', { class: 'num' }, row.credits ? formatCredit(row.credits) : '0.00') },
  {
    title: '活动状态', key: 'campaigns',
    render: row => {
      if (row.campaigns?.length) {
        return h('div', { class: 'campaign-list' }, row.campaigns.map(campaign =>
          h('div', { class: 'campaign-row', title: campaign.message || '' }, [
            h('span', { class: 'campaign-title' }, campaign.title || `活动 ${campaign.campaign_id}`),
            h(UiTag, { type: campaignStatusType(campaign.status) }, { default: () => campaignStatusLabel(campaign.status) }),
            h('span', { class: 'campaign-message' }, campaign.message || '—')
          ])
        ))
      }
      return h('span', { class: 'muted' }, row.message || '无活动')
    }
  }
]

const accountColumns = [
  {
    title: '账号', key: 'nickname', minWidth: 210,
    render: row => h('div', { class: 'cell-two-line' }, [
      h('strong', {}, row.nickname || '未命名'),
      h('span', { class: 'sub mono' }, row.uid || row.name || '—')
    ])
  },
  {
    title: '状态', key: 'status', width: 78,
    render: row => h(UiTag, {
      type: row.disabled ? 'error' : row.cooling ? 'warning' : 'success',
      title: row.reason || undefined
    }, { default: () => (row.disabled ? '禁用' : row.cooling ? '冷却' : '健康') })
  },
  { title: '并发', key: 'active_concurrent', width: 76, align: 'right', render: row => `${row.active_concurrent}/${row.max_concurrent}` },
  { title: '错误', key: 'err_count', width: 68, align: 'right', render: row => h('span', { class: 'num' }, formatNumber(row.err_count)) },
  {
    title: '剩余积分', key: 'credit_balance', width: 112, align: 'right',
    render: row => {
      const balance = row.credit_balance
      if (!balance) return '—'
      if (!balance.ok) {
        return h(UiTag, { type: 'error', title: balance.message }, { default: () => '查询失败' })
      }
      return h('strong', {
        class: 'credit-value',
        title: `总额 ${formatCredit(balance.total_amount)}，已用 ${formatCredit(balance.used_amount)}`
      }, formatCredit(balance.remaining_amount))
    }
  },
  { title: 'Token 剩余', key: 'token_remaining', width: 102, align: 'right', render: row => h('span', { title: 'Token 有效期剩余' }, formatRemaining(row.token_remaining)) },
  { title: '到期时间', key: 'expires_at', width: 150, render: row => h('span', { class: 'num' }, formatTime(row.expires_at)) },
  {
    title: '最近签到', key: 'checkin', width: 108,
    render: row => row.checkin
      ? h(UiTag, { type: row.checkin.ok ? 'success' : 'error', title: row.checkin.message }, { default: () => formatTime(row.checkin.checked_at) })
      : '—'
  },
  {
    title: '操作', key: 'actions', width: 320, fixed: 'right',
    render: row => {
      const loadingAction = accountActionLoading.value[row.uid]
      const feedback = accountActionFeedback.value[row.uid]
      const confirmingDelete = pendingDeleteUID.value === row.uid
      const btns = [
        h(UiButton, {
          size: 'xs', variant: 'outline',
          loading: loadingAction === 'validate',
          disabled: Boolean(loadingAction) && loadingAction !== 'validate',
          onClick: () => runAccountAction(row, 'validate')
        }, { default: () => '校验' }),
        h(UiButton, {
          size: 'xs', variant: 'outline',
          loading: loadingAction === 'checkin',
          disabled: Boolean(loadingAction) && loadingAction !== 'checkin',
          onClick: () => runAccountAction(row, 'checkin')
        }, { default: () => '签到' }),
        h(UiButton, {
          size: 'xs', variant: 'outline',
          loading: loadingAction === 'keepalive',
          disabled: Boolean(loadingAction) && loadingAction !== 'keepalive',
          onClick: () => runAccountAction(row, 'keepalive')
        }, { default: () => '保活' })
      ]
      if (row.cooling) {
        btns.push(h(UiButton, {
          size: 'xs', variant: 'outline',
          loading: loadingAction === 'clearCooldown',
          disabled: Boolean(loadingAction) && loadingAction !== 'clearCooldown',
          onClick: () => runAccountAction(row, 'clearCooldown')
        }, { default: () => '清冷却' }))
      }
      if (row.disabled) {
        btns.push(h(UiButton, {
          size: 'xs', variant: 'primary',
          loading: loadingAction === 'enable',
          disabled: Boolean(loadingAction) && loadingAction !== 'enable',
          onClick: () => runAccountAction(row, 'enable')
        }, { default: () => '启用' }))
      } else {
        btns.push(h(UiButton, {
          size: 'xs', variant: 'outline',
          loading: loadingAction === 'disable',
          disabled: Boolean(loadingAction) && loadingAction !== 'disable',
          onClick: () => runAccountAction(row, 'disable')
        }, { default: () => '禁用' }))
      }
      if (confirmingDelete) {
        btns.push(
          h(UiButton, {
            size: 'xs', variant: 'danger',
            loading: loadingAction === 'delete',
            disabled: Boolean(loadingAction) && loadingAction !== 'delete',
            onClick: () => confirmDeleteAccount(row)
          }, { default: () => '确认删除' }),
          h(UiButton, {
            size: 'xs', variant: 'ghost',
            disabled: Boolean(loadingAction),
            onClick: cancelDeleteAccount
          }, { default: () => '取消' })
        )
      } else {
        btns.push(h(UiButton, {
          size: 'xs', variant: 'danger',
          loading: loadingAction === 'delete',
          disabled: Boolean(loadingAction) && loadingAction !== 'delete',
          onClick: () => askDeleteAccount(row.uid)
        }, { default: () => '删除' }))
      }
      return h('div', { class: 'account-actions' }, [
        h('div', { class: 'row' }, btns),
        feedback
          ? h('span', { class: ['account-action-feedback', feedback.type], title: feedback.message }, feedback.message)
          : null
      ])
    }
  }
]

onMounted(() => {
  if (loggedIn.value) loadCreditBalances()
})
</script>

<template>
  <div class="page">
    <UiCard title="账号池" flush>
      <template #header-extra>
        <UiTag v-if="creditLoaded" type="success" :title="`总额 ${formatCredit(creditTotal.total_amount)}，已用 ${formatCredit(creditTotal.used_amount)}`">
          总剩余 {{ formatCredit(creditTotal.remaining_amount) }}
        </UiTag>
        <UiButton variant="outline" size="sm" @click="batchCheckinOpen = true">批量签到</UiButton>
        <UiButton variant="outline" size="sm" :loading="creditLoading" @click="loadCreditBalances">刷新余额</UiButton>
        <UiButton variant="outline" size="sm" :loading="reloadLoading" @click="reloadAccountsFromDisk">重载账号</UiButton>
        <a
          v-if="oauthSession?.auth_url" class="u-btn u-btn--outline u-btn--sm"
          :href="oauthSession.auth_url" target="_blank" rel="noopener"
        >
          <Icon name="external-link" :size="14" />
          授权页
        </a>
        <UiButton variant="primary" size="sm" :loading="oauthLoading" @click="startAccountLogin">
          <Icon name="user-plus" :size="14" />
          添加账号
        </UiButton>
        <UiTag>{{ formatNumber(accountRows.length) }}</UiTag>
      </template>

      <div v-if="oauthStatus || oauthSession" class="oauth-panel" style="padding: 14px 18px 0">
        <UiAlert v-if="oauthStatus" :type="oauthError ? 'error' : 'info'">{{ oauthStatus }}</UiAlert>
        <div v-if="oauthSession" class="row">
          <UiInput v-model="oauthCallbackURL" :placeholder="CALLBACK_HINT" @keyup.enter="importAccountCallback" />
          <UiButton v-if="oauthCallbackURL" :loading="oauthLoading" @click="importAccountCallback">导入回调</UiButton>
        </div>
      </div>

      <div v-if="poolFeedback" style="padding: 14px 18px 0">
        <UiAlert :type="poolFeedback.type">{{ poolFeedback.message }}</UiAlert>
      </div>

      <div v-if="creditError" style="padding: 14px 18px 0">
        <UiAlert type="warning">{{ creditError }}</UiAlert>
      </div>

      <div class="filters-bar">
        <div class="filters-bar-left">
          <UiInput v-model="accountSearch" clearable size="sm" placeholder="搜索账号昵称、名称或 UID…" style="width: 280px" />
        </div>
        <div class="filters-bar-right">
          <span v-if="creditLoaded" class="credit-summary muted">
            总额 <strong class="num">{{ formatCredit(creditTotal.total_amount) }}</strong> ·
            已用 <strong class="num">{{ formatCredit(creditTotal.used_amount) }}</strong> ·
            剩余 <strong class="num" style="color: var(--success)">{{ formatCredit(creditTotal.remaining_amount) }}</strong>
          </span>
        </div>
      </div>

      <UiDataTable
        :columns="accountColumns" :data="filteredAccounts" :loading="loading" :scroll-x="1280"
        :row-key="row => row.uid"
      />
    </UiCard>

    <UiDrawer v-model="batchCheckinOpen" title="批量签到" :width="760">
      <div class="batch-toolbar">
        <UiSelect v-model="batchCheckinSize" :options="batchSizeOptions" style="width: 170px" />
        <div class="batch-actions">
          <UiButton :loading="batchCheckinLoading" @click="loadBatchCheckinStatus">查询状态</UiButton>
          <UiButton variant="primary" :loading="batchCheckinLoading" @click="runBatchCheckin">开始签到</UiButton>
        </div>
      </div>
      <UiAlert v-if="batchCheckinMessage" :type="batchCheckinError ? 'warning' : 'success'" style="margin-bottom: 14px">
        {{ batchCheckinMessage }}
      </UiAlert>
      <UiDataTable
        :columns="batchCheckinColumns" :data="batchCheckinResults" :loading="batchCheckinLoading" :scroll-x="660"
        :row-key="row => row.uid"
        :pagination="{
          local: true,
          page: batchCheckinPage,
          pageSize: batchCheckinPageSize,
          itemCount: batchCheckinResults.length,
          onUpdatePage: p => { batchCheckinPage = p },
          onUpdatePageSize: s => { batchCheckinPageSize = s; batchCheckinPage = 1 }
        }"
      />
    </UiDrawer>
  </div>
</template>

<style scoped>
.credit-summary { font-size: 12.5px; }
.credit-summary strong { color: var(--text); font-weight: 650; }
.batch-toolbar {
  display: flex; align-items: center; justify-content: space-between; gap: 10px;
  flex-wrap: wrap; margin-bottom: 14px;
}
.batch-actions { display: flex; gap: 8px; }
</style>
