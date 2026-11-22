<script setup>
import { onMounted, ref } from 'vue'
import UiAlert from '../components/ui/UiAlert.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiCard from '../components/ui/UiCard.vue'
import UiInput from '../components/ui/UiInput.vue'
import UiInputNumber from '../components/ui/UiInputNumber.vue'
import UiSwitch from '../components/ui/UiSwitch.vue'
import { loadAll, observabilityDisabled, request } from '../store'

const settingsLoading = ref(false)
const settingsSaving = ref(false)
const settingsMessage = ref('')
const settingsError = ref(false)
const settingsLoaded = ref(false)
const settingsForm = ref(emptySettings())

const purgeLoading = ref(false)
const purgeMessage = ref('')
const purgeError = ref(false)

function emptySettings() {
  return {
    default_model: 'glm-5.2',
    max_concurrent: 1,
    queue_retry_seconds: 10,
    queue_max_attempts: 30,
    benefit_auto_claim: true,
    credits_auto_claim: true,
    watch_enabled: true,
    poll_minutes: 30,
    refresh_skew_minutes: 30,
    keepalive_interval_minutes: 15,
    keepalive_window_minutes: 10,
    models_return_benefit: true,
    models_hidden: ''
  }
}

async function loadSettings() {
  settingsLoading.value = true
  try {
    const result = await request('/admin/api/settings')
    settingsForm.value = { ...emptySettings(), ...result.settings }
    settingsLoaded.value = true
    settingsMessage.value = ''
  } catch (error) {
    settingsError.value = true
    settingsMessage.value = error.message
  } finally {
    settingsLoading.value = false
  }
}

async function saveSettings() {
  if (!settingsLoaded.value || settingsLoading.value) return
  settingsSaving.value = true
  settingsMessage.value = ''
  try {
    const result = await request('/admin/api/settings', {
      method: 'POST',
      body: JSON.stringify(settingsForm.value)
    })
    settingsForm.value = { ...result.settings }
    settingsLoaded.value = true
    settingsError.value = false
    settingsMessage.value = result.message || '设置已保存'
    await loadAll()
  } catch (error) {
    settingsError.value = true
    settingsMessage.value = error.message
  } finally {
    settingsSaving.value = false
  }
}

async function purgeObservability() {
  purgeLoading.value = true
  purgeMessage.value = ''
  purgeError.value = false
  try {
    const result = await request('/admin/api/observability/purge', { method: 'POST', body: JSON.stringify({}) })
    const stats = result.stats || {}
    purgeMessage.value = `${result.message || '已清理'}（请求日志 ${stats.requests ?? 0} 条、事件 ${stats.events ?? 0} 条、快照 ${stats.snapshots ?? 0} 条）`
    await loadAll()
  } catch (error) {
    purgeError.value = true
    purgeMessage.value = `清理失败：${error.message}`
  } finally {
    purgeLoading.value = false
  }
}

onMounted(loadSettings)
</script>

<template>
  <div class="page">
    <UiCard title="运行设置">
      <template #header-extra>
        <UiButton variant="outline" size="sm" :loading="settingsLoading" @click="loadSettings">重新加载</UiButton>
        <UiButton variant="primary" size="sm" :loading="settingsSaving" :disabled="settingsLoading || !settingsLoaded" @click="saveSettings">保存设置</UiButton>
      </template>

      <UiAlert v-if="settingsMessage" :type="settingsError ? 'error' : 'success'" style="margin-bottom: 18px">
        {{ settingsMessage }}
      </UiAlert>

      <div class="settings-sections" :class="{ 'is-busy': settingsLoading || settingsSaving }">
        <!-- 1. 模型与目录 -->
        <section class="settings-section">
          <div class="section-head">
            <h4>模型与目录</h4>
            <p>控制默认推理模型及 /v1/models 返回的模型列表过滤策略</p>
          </div>
          <div class="settings-grid">
            <div class="form-item">
              <label>默认模型</label>
              <UiInput v-model="settingsForm.default_model" placeholder="glm-5.2" />
            </div>
            <div class="form-item">
              <label>福利模型</label>
              <div class="switch-item">
                <UiSwitch v-model="settingsForm.models_return_benefit" />
                <span>在 /v1/models 返回限时福利模型</span>
              </div>
            </div>
            <div class="form-item full">
              <label>隐藏指定模型</label>
              <UiInput
                v-model="settingsForm.models_hidden"
                style="width: min(560px, 100%)"
                placeholder="逗号分隔的模型 ID，如 GLM-5.2-ArkTS-SPARK, OpenPangu-2.0-Pro"
              />
            </div>
          </div>
        </section>

        <!-- 2. 并发与排队 -->
        <section class="settings-section">
          <div class="section-head">
            <h4>并发与排队</h4>
            <p>控制单账号并发容量与繁忙时的请求排队重试策略</p>
          </div>
          <div class="settings-grid">
            <div class="form-item">
              <label>单账号并发上限</label>
              <UiInputNumber v-model="settingsForm.max_concurrent" :min="1" :max="10" />
            </div>
            <div class="form-item">
              <label>排队重试间隔（秒）</label>
              <UiInputNumber v-model="settingsForm.queue_retry_seconds" :min="1" :max="300" />
            </div>
            <div class="form-item">
              <label>排队最大尝试</label>
              <UiInputNumber v-model="settingsForm.queue_max_attempts" :min="1" :max="180" />
            </div>
          </div>
        </section>

        <!-- 3. 调度与保活 -->
        <section class="settings-section">
          <div class="section-head">
            <h4>调度与保活</h4>
            <p>配置 Watchdog 轮询周期、Token 提前刷新及空闲保活心跳</p>
          </div>
          <div class="settings-grid">
            <div class="form-item">
              <label>调度开关</label>
              <div class="switch-item">
                <UiSwitch v-model="settingsForm.watch_enabled" />
                <span>启用 Watchdog 轮询</span>
              </div>
            </div>
            <div class="form-item">
              <label>轮询间隔（分钟）</label>
              <UiInputNumber v-model="settingsForm.poll_minutes" :min="1" :max="1440" />
            </div>
            <div class="form-item">
              <label>Token 提前刷新（分钟）</label>
              <UiInputNumber v-model="settingsForm.refresh_skew_minutes" :min="1" :max="720" />
            </div>
            <div class="form-item">
              <label>保活心跳间隔（分钟）</label>
              <UiInputNumber v-model="settingsForm.keepalive_interval_minutes" :min="1" :max="1440" />
            </div>
            <div class="form-item">
              <label>保活空闲窗口（分钟）</label>
              <UiInputNumber v-model="settingsForm.keepalive_window_minutes" :min="1" :max="1440" />
            </div>
          </div>
        </section>

        <!-- 4. 自动领取 -->
        <section class="settings-section">
          <div class="section-head">
            <h4>自动领取</h4>
            <p>独立控制每日签到/新手积分与限时福利模型的自动领取行为</p>
          </div>
          <div class="settings-grid">
            <div class="form-item">
              <label>活动积分</label>
              <div class="switch-item">
                <UiSwitch v-model="settingsForm.credits_auto_claim" />
                <span>自动领取每日签到及新手积分</span>
              </div>
            </div>
            <div class="form-item">
              <label>限时福利</label>
              <div class="switch-item">
                <UiSwitch v-model="settingsForm.benefit_auto_claim" />
                <span>发现模型时自动领取限时福利</span>
              </div>
            </div>
          </div>
        </section>
      </div>
    </UiCard>

    <UiCard title="运维与数据维护">
      <template #header-extra>
        <UiButton
          variant="outline"
          size="sm"
          :loading="purgeLoading"
          :disabled="observabilityDisabled"
          @click="purgeObservability"
        >
          清理观测数据
        </UiButton>
      </template>

      <UiAlert v-if="observabilityDisabled" type="info" style="margin-bottom: 12px">
        当前服务端已关闭可观测数据库（observability.enabled = false），无需清理。
      </UiAlert>
      <UiAlert v-if="purgeMessage" :type="purgeError ? 'error' : 'success'" style="margin-bottom: 12px">
        {{ purgeMessage }}
      </UiAlert>
      <p class="maint-desc">
        按服务端保留策略（retention_days / max_rows / max_db_bytes）立即清理过期的请求日志、系统事件与账号快照。
      </p>
    </UiCard>
  </div>
</template>

<style scoped>
.settings-sections { display: grid; gap: 22px; }
.settings-sections.is-busy { opacity: 0.6; pointer-events: none; }
.settings-section {
  padding-bottom: 22px;
  border-bottom: 1px solid var(--border);
  display: grid;
  gap: 14px;
}
.settings-section:last-child { padding-bottom: 4px; border-bottom: none; }
.section-head { display: grid; gap: 3px; }
.section-head h4 { font-size: 14px; font-weight: 650; color: var(--text); }
.section-head p { margin: 0; font-size: 12.5px; color: var(--text-3); }
.maint-desc { margin: 0; font-size: 13px; color: var(--text-2); }
</style>
