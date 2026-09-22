<script setup lang="ts">
// App.vue is the view. State and behaviour live in the composables so each
// concern can be read on its own:
//   useService.ts  service configuration, tool paths, image upload
//   usePlans.ts    production plans: create, select, run, delete
//   useJobs.ts     job list, live monitor (SSE), cancel/resume/download
//   api.ts         every HTTP call
//   types.ts       wire shapes; format.ts display helpers

import { onMounted, ref } from 'vue'
import { api, errorText } from './api'
import { useJobs } from './useJobs'
import { usePlans } from './usePlans'
import { useService } from './useService'
import type { Job, Plan } from './types'
import { PHASES, formatBytes, phaseIndex, phaseLabel, statusText } from './format'

const errorMessage = ref('')

function reportError(message: string) {
  errorMessage.value = message
}

const service = useService(reportError)

// Created before usePlans because the plan list needs to know which plans are
// busy, so that a running plan cannot be selected for deletion.
const jobStore = useJobs(reportError)

const planStore = usePlans({
  onError: reportError,
  onJobStarted: (job: Job) => jobStore.selectJob(job),
  onPlansDeleted: (ids: string[]) => {
    // If the plan that owns the selected job is gone, drop the monitor too.
    const selected = jobStore.job.value
    if (selected?.plan_id && ids.includes(selected.plan_id)) jobStore.clearSelection()
    if (ids.includes(planStore.selectedPlanId.value)) planStore.selectedPlanId.value = ''
  },
  runningPlanIds: () => jobStore.runningPlanIds.value,
  // The uploaded image set belongs to useService; usePlans only reads it.
  upload: () => service.upload.value,
  formatBytes,
})

async function refresh() {
  try {
    const [serviceConfig, planResponse, jobResponse] = await Promise.all([
      api.config(), api.plans(), api.jobs(),
    ])
    service.applyConfig(serviceConfig)
    planStore.plans.value = planResponse.plans
    jobStore.jobs.value = jobResponse.jobs
    // Adopt the most recent job on first load so the monitor is not empty.
    if (!jobStore.job.value && jobResponse.jobs.length) jobStore.selectJob(jobResponse.jobs[0])
  } catch (error) {
    reportError(errorText(error, '无法连接服务'))
  }
}

async function runPlan(plan: Plan) {
  await planStore.runPlan(plan)
  try {
    await jobStore.refreshJobs()
  } catch (error) {
    reportError(errorText(error, '刷新任务列表失败'))
  }
}

// The template reads these names directly. Destructuring keeps the markup flat
// rather than prefixing every reference with its composable, which would make
// the template harder to scan than the original single-file version.
const {
  config, toolDraft, showToolSettings, isSavingTools, installCommandCopied,
  selectedFiles, upload, isUploading, ready, canUpload,
  saveToolPaths, chooseFiles, uploadFiles, copyInstallCommand,
} = service

const {
  plans, planName, scheduledAt, sourceMode, inputPath, isCreating,
  checkedPlanIds, selectedPlanId, isDeleting, deleteNotice,
  checkedCount, selectablePlans, allSelected, canDeleteChecked, canCreate,
  selectPlan, togglePlanCheck, toggleCheckAll, createPlan, deleteCheckedPlans,
} = planStore

const { job, isRunning, runningPlanIds, cancelJob, resumeJob, download } = jobStore

// The template iterates the phase list and resolves a phase key to its index.
const phases = PHASES

onMounted(refresh)
</script>


<template>
  <div class="shell">
    <header class="topbar"><div><p class="eyebrow">DOCKER-READY PHOTOGRAMMETRY SERVICE</p><h1>航飞影像 · Smart3D OSGB 生产</h1></div><div class="top-actions"><button class="settings" @click="showToolSettings = true">工具配置</button><div class="service-status" :class="{ ready }"><span class="status-dot"></span>{{ ready ? '生产引擎就绪' : '等待重建引擎配置' }}</div></div></header>
    <main class="workspace">
      <aside class="control-panel">
        <div class="section-heading"><div><p class="eyebrow">CREATE PLAN</p><h2>创建生产计划</h2></div><span class="local-badge">容器化</span></div>
        <label class="field"><span>计划名称</span><input v-model="planName" placeholder="例如：矿区 2026-09-21 航飞重建" /></label>
        <div class="source-tabs"><button :class="{ active: sourceMode === 'upload' }" @click="sourceMode = 'upload'">上传影像</button><button :class="{ active: sourceMode === 'path' }" @click="sourceMode = 'path'">挂载目录</button></div>
        <section v-if="sourceMode === 'upload'" class="upload-box"><input id="image-folder" type="file" multiple webkitdirectory @change="chooseFiles" /><label for="image-folder" class="upload-label"><strong>选择航飞影像文件夹</strong><span>支持 JPG / PNG / TIFF / WebP，保留原目录结构</span></label><p v-if="selectedFiles.length" class="upload-summary">已选择 {{ selectedFiles.length }} 个文件 · {{ formatBytes(selectedFiles.reduce((sum, file) => sum + file.size, 0)) }}</p><button class="secondary wide" :disabled="!canUpload" @click="uploadFiles">{{ isUploading ? '正在上传…' : upload ? '重新上传影像' : '上传并校验影像' }}</button><p v-if="upload" class="ok-note">已就绪：{{ upload.files }} 张影像，{{ formatBytes(upload.bytes) }}</p></section>
        <label v-else class="field"><span>容器内影像目录</span><input v-model="inputPath" placeholder="例如 /mnt/input/site-a/images" /><small>该目录须通过 docker compose 挂载到容器。</small></label>
        <label class="field"><span>预约执行时间（可选）</span><input v-model="scheduledAt" type="datetime-local" /><small>留空则保存为草稿；可从右侧计划列表手动启动。</small></label>
        <div class="action-grid"><button class="secondary" :disabled="!canCreate" @click="createPlan(false)">保存计划</button><button class="primary-action" :disabled="!canCreate" @click="createPlan(true)">{{ isCreating ? '创建中…' : '立即重建 OSGB' }}</button></div>
        <p v-if="errorMessage" class="error-banner">{{ errorMessage }}</p>
        <p v-if="deleteNotice" class="ok-note">{{ deleteNotice }}</p>
        <section class="plan-list">
          <div class="list-heading">
            <span>已保存计划</span>
            <span>{{ plans.length }}</span>
          </div>
          <div v-if="plans.length" class="plan-toolbar">
            <label class="check-all">
              <input type="checkbox" :checked="selectablePlans.length > 0 && checkedCount === selectablePlans.length" :indeterminate.prop="checkedCount > 0 && checkedCount < selectablePlans.length" :disabled="!selectablePlans.length" @change="toggleCheckAll(($event.target as HTMLInputElement).checked)" />
              <span>全选</span>
            </label>
            <button class="danger compact" :disabled="!canDeleteChecked" @click="deleteCheckedPlans">{{ isDeleting ? '删除中…' : `删除选中${checkedCount ? `（${checkedCount}）` : ''}` }}</button>
          </div>
          <div v-if="!plans.length" class="empty-text">尚无计划</div>
          <div v-for="plan in plans" :key="plan.id" class="plan-row" :class="{ active: selectedPlanId === plan.id }">
            <label class="plan-check" :title="runningPlanIds.has(plan.id) ? '任务运行中，无法删除' : '选择此计划'">
              <input type="checkbox" :checked="checkedPlanIds.has(plan.id)" :disabled="runningPlanIds.has(plan.id)" @change="togglePlanCheck(plan.id, ($event.target as HTMLInputElement).checked)" />
            </label>
            <button class="plan-item" @click="selectPlan(plan)">
              <span><strong>{{ plan.name }}</strong><small>{{ plan.upload_id ? '已上传影像' : plan.input_path }}</small></span>
              <em :class="plan.status">{{ statusText(plan.status) }}</em>
            </button>
            <button class="run-plan compact" :disabled="runningPlanIds.has(plan.id)" :title="runningPlanIds.has(plan.id) ? '任务运行中' : '开始重建'" @click="runPlan(plan)">{{ runningPlanIds.has(plan.id) ? '运行中' : '开始' }}</button>
          </div>
        </section>
      </aside>
      <section class="monitor-panel">
        <div class="monitor-header"><div><p class="eyebrow">LIVE JOB MONITOR</p><h2>{{ job ? `任务 ${job.id.slice(-12)}` : '等待任务' }}</h2></div><div class="monitor-actions"><button v-if="isRunning" class="secondary compact" @click="cancelJob">停止任务</button><button v-if="job?.status === 'failed' && job.resumable" class="primary-action compact" @click="resumeJob">从检查点继续</button><button v-if="job?.status === 'completed'" class="download" @click="download">下载 OSGB 成果包</button><span class="job-status" :class="job?.status">{{ statusText(job?.status) }}</span></div></div>
        <div v-if="job?.status === 'failed'" class="job-error-panel" role="alert"><div class="job-error-title"><strong>转换失败</strong><span>失败阶段：{{ phases[phaseIndex(job.phase)]?.[1] ?? job.phase }}</span></div><p>{{ job.error || '任务异常终止，请查看实时日志。' }}</p><div class="job-error-meta"><span>任务：{{ job.id }}</span><span>进度：{{ job.progress }}%</span><span v-if="job.checkpoint">最近检查点：{{ job.checkpoint }}</span></div></div>
        <div class="progress-block"><div class="progress-meta"><span>{{ job ? phases[phaseIndex(job.phase)]?.[1] ?? job.phase : '尚未开始' }}</span><strong>{{ job?.progress ?? 0 }}%</strong></div><div class="progress-track"><div class="progress-value" :class="{ failed: job?.status === 'failed' }" :style="{ width: `${job?.progress ?? 0}%` }"></div></div></div>
        <div class="timeline"><div v-for="([key, label], index) in phases" :key="key" class="timeline-item" :class="{ completed: job && phaseIndex(job.phase) > index, current: job?.phase === key && job.status !== 'failed', failed: job?.phase === key && job.status === 'failed', pending: !job || phaseIndex(job.phase) < index }"><span class="timeline-dot"><template v-if="job?.phase === key && job.status === 'failed'">×</template><template v-else-if="job && phaseIndex(job.phase) > index">✓</template><template v-else>{{ index + 1 }}</template></span><span>{{ label }}</span></div></div>
        <div v-if="job" class="job-facts"><div><span>影像输入</span><strong>{{ job.input_count }} 张 · {{ formatBytes(job.input_bytes) }}</strong></div><div><span>成果校验</span><strong>{{ job.stats ? `瓦片 ${job.stats.valid_tiles ?? job.stats.tiles ?? 0} / ${job.stats.tiles ?? 0}，含纹理与 metadata` : '等待校验' }}</strong></div><div class="fact-wide"><span>产物路径</span><strong class="path-value">{{ job.output_path }}</strong></div></div>
        <div class="log-section"><div class="log-heading"><span>实时日志</span><span>{{ job?.logs.length ?? 0 }} 条</span></div><div class="log-view" aria-live="polite"><div v-if="!job?.logs.length" class="empty-log">计划启动后，将持续显示重建命令、LOD 组态和完整性校验结果。</div><div v-for="(entry, index) in job?.logs ?? []" :key="`${entry.at}-${index}`" class="log-line" :class="[entry.stream, { fatal: entry.stream === 'fatal' || entry.message.includes('signal: killed') || entry.message.includes('command colmap failed') }]"><time>{{ new Date(entry.at).toLocaleTimeString() }}</time><span class="stream-tag">{{ entry.stream === 'fatal' ? '致命错误' : entry.stream === 'stderr' ? '工具输出' : entry.stream }}</span><span>{{ entry.message }}</span></div></div></div>
        <div v-if="job?.status === 'completed'" class="success-banner"><span class="success-icon">✓</span><div><strong>Smart3D 分层 OSGB 已生成并回读校验</strong><span>下载包包含 root.osgb、Data/、纹理和 validation_report.json。</span></div></div>
      </section>
    </main>
    <div v-if="showToolSettings" class="modal-backdrop" @click.self="showToolSettings = false">
      <section class="settings-modal" role="dialog" aria-modal="true" aria-label="工具配置">
        <div class="modal-header"><div><p class="eyebrow">TOOL CONFIGURATION</p><h2>工具位置检测与配置</h2></div><button class="modal-close" @click="showToolSettings = false">×</button></div>
        <p class="modal-note">配置保存至 <code>{{ config?.config_file }}</code>。保存会重新检测可执行文件；任务运行或排队时禁止修改。</p>
        <div v-if="config?.deployment?.kind === 'docker'" class="install-callout">
          <strong>Docker/Linux 自动安装</strong>
          <span>{{ config.deployment.note }}</span>
          <code>{{ config.deployment.tool_install_command }}</code>
          <button class="secondary compact" @click="copyInstallCommand">{{ installCommandCopied ? '已复制' : '复制一键安装命令' }}</button>
        </div>
        <div v-for="tool in config?.tool_paths ?? []" :key="tool.key" class="tool-row">
          <label><span>{{ tool.label }}<i v-if="tool.required">必需</i></span><input v-model="toolDraft[tool.key]" :placeholder="tool.key" /></label>
          <span class="tool-state" :class="{ available: tool.available }">{{ tool.available ? '已检测' : '未检测到' }}</span>
        </div>
        <div class="modal-actions"><button class="secondary" @click="showToolSettings = false">取消</button><button class="primary-action modal-save" :disabled="isSavingTools" @click="saveToolPaths">{{ isSavingTools ? '检测并保存中…' : '保存并检测' }}</button></div>
      </section>
    </div>
  </div>
</template>
