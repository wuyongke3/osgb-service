<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

type LogEntry = { at: string; stream: string; message: string }
type Job = {
  id: string; status: 'queued' | 'running' | 'completed' | 'failed'; phase: string; progress: number
  created_at: string; updated_at: string; input_path: string; output_path: string; input_count: number
  input_bytes: number; plan_id?: string; source_type?: string; error?: string; logs: LogEntry[]
  checkpoint?: string; resumable?: boolean
  stats?: { vertices: number; faces: number; tiles?: number; valid_tiles?: number; failed_tiles?: number; textured?: boolean }
}
type Plan = { id: string; name: string; upload_id?: string; input_path?: string; status: string; scheduled_at?: string; created_at: string; updated_at: string; last_job_id?: string }
type Upload = { id: string; path: string; files: number; bytes: number; created_at: string }
type ToolPath = { key: string; label: string; value: string; available: boolean; required: boolean }
type ServiceConfig = { native_pipeline_ready: boolean; dependencies: Record<string, boolean>; pipeline_mode: string; output_root: string; config_file: string; native_directory_picker: boolean; tool_paths: ToolPath[]; deployment?: { kind: string; tool_install_mode: string; tool_install_command: string; automatic: boolean; can_execute_here: boolean; note: string } }

const config = ref<ServiceConfig | null>(null)
const plans = ref<Plan[]>([])
const jobs = ref<Job[]>([])
const job = ref<Job | null>(null)
const sourceMode = ref<'upload' | 'path'>('upload')
const selectedFiles = ref<File[]>([])
const inputPath = ref('')
const upload = ref<Upload | null>(null)
const planName = ref('')
const scheduledAt = ref('')
const errorMessage = ref('')
const isUploading = ref(false)
const isCreating = ref(false)
const isSavingTools = ref(false)
const showToolSettings = ref(false)
const toolDraft = ref<Record<string, string>>({})
const installCommandCopied = ref(false)
// Plans ticked in the left list. Selection is kept as a Set of ids so it
// survives the list being refreshed after a delete.
const checkedPlanIds = ref<Set<string>>(new Set())
const selectedPlanId = ref('')
const isDeleting = ref(false)
const deleteNotice = ref('')
let eventSource: EventSource | null = null

const phases = [
  ['preparing', '影像预处理'], ['features', '特征提取'], ['matching', '特征匹配'], ['sfm', '空三 / 束调'],
  ['dense', '稠密点云'], ['mesh', '网格重建'], ['texture', '纹理映射'], ['georeference', '坐标检查'],
  ['lod', 'Smart3D 分层 OSGB'], ['osgb_conversion', 'OSGB 转换与校验'], ['completed', '完成'],
] as const

const isRunning = computed(() => job.value?.status === 'queued' || job.value?.status === 'running')
const ready = computed(() => Boolean(config.value?.native_pipeline_ready))
const canUpload = computed(() => selectedFiles.value.length > 0 && !isUploading.value)
const canCreate = computed(() => Boolean(planName.value.trim()) && (sourceMode.value === 'upload' ? Boolean(upload.value) : Boolean(inputPath.value.trim())) && !isCreating.value)
function phaseIndex(phase?: string) { const i = phases.findIndex(([key]) => key === phase); return i < 0 ? 0 : i }
function formatBytes(bytes = 0) { return bytes < 1024 ** 3 ? `${(bytes / 1024 ** 2).toFixed(1)} MB` : `${(bytes / 1024 ** 3).toFixed(2)} GB` }
function statusText(status?: string) { return ({ queued: '排队中', running: '处理中', completed: '已完成', failed: '失败', scheduled: '已预约', draft: '草稿' } as Record<string, string>)[status ?? ''] ?? '待处理' }
async function request<T>(url: string, options?: RequestInit): Promise<T> {
  const response = await fetch(url, options); const data = await response.json()
  if (!response.ok) throw new Error(data.detail || '请求失败'); return data as T
}
async function refresh() {
  try {
    const [service, planResponse, jobResponse] = await Promise.all([request<ServiceConfig>('/api/config'), request<{ plans: Plan[] }>('/api/plans'), request<{ jobs: Job[] }>('/api/jobs')])
    config.value = service; toolDraft.value = Object.fromEntries(service.tool_paths.map(tool => [tool.key, tool.value])); plans.value = planResponse.plans; jobs.value = jobResponse.jobs
    if (!job.value && jobs.value.length) selectJob(jobs.value[0])
  } catch (error) { errorMessage.value = error instanceof Error ? error.message : '无法连接服务' }
}
async function saveToolPaths() {
  if (!config.value) return
  isSavingTools.value = true; errorMessage.value = ''
  try {
    config.value = await request<ServiceConfig>('/api/config', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ tools: toolDraft.value }) })
    toolDraft.value = Object.fromEntries(config.value.tool_paths.map(tool => [tool.key, tool.value]))
    showToolSettings.value = false
  } catch (error) { errorMessage.value = error instanceof Error ? error.message : '工具配置保存失败' } finally { isSavingTools.value = false }
}
function chooseFiles(event: Event) { selectedFiles.value = Array.from((event.target as HTMLInputElement).files ?? []); upload.value = null }
async function uploadFiles() {
  if (!canUpload.value) return; isUploading.value = true; errorMessage.value = ''
  try { const form = new FormData(); for (const file of selectedFiles.value) form.append('files', file, file.webkitRelativePath || file.name); upload.value = await request<Upload>('/api/uploads', { method: 'POST', body: form }) }
  catch (error) { errorMessage.value = error instanceof Error ? error.message : '上传失败' } finally { isUploading.value = false }
}
async function createPlan(startNow: boolean) {
  if (!canCreate.value) return; isCreating.value = true; errorMessage.value = ''
  try {
    const body: Record<string, unknown> = { name: planName.value.trim(), start_now: startNow }
    if (sourceMode.value === 'upload') body.upload_id = upload.value?.id; else body.input_path = inputPath.value.trim()
    if (!startNow && scheduledAt.value) body.scheduled_at = new Date(scheduledAt.value).toISOString()
    const result = await request<Plan | { plan: Plan; job: Job }>('/api/plans', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
    if ('job' in result) { selectJob(result.job); plans.value = [result.plan, ...plans.value.filter(item => item.id !== result.plan.id)] } else plans.value = [result, ...plans.value]
    planName.value = ''; scheduledAt.value = ''
  } catch (error) { errorMessage.value = error instanceof Error ? error.message : '创建计划失败' } finally { isCreating.value = false }
}
async function runPlan(plan: Plan) { try { selectJob(await request<Job>(`/api/plans/${encodeURIComponent(plan.id)}/run`, { method: 'POST' })); await refresh() } catch (error) { errorMessage.value = error instanceof Error ? error.message : '启动计划失败' } }

// Plans are selected, not started, when clicked. Starting is a deliberate
// second click because a run occupies the pipeline for hours.
function selectPlan(plan: Plan) { selectedPlanId.value = plan.id; errorMessage.value = ''; deleteNotice.value = '' }
function togglePlanCheck(planId: string, checked: boolean) {
  const next = new Set(checkedPlanIds.value)
  if (checked) next.add(planId); else next.delete(planId)
  checkedPlanIds.value = next
}
function clearCheckedPlans() { checkedPlanIds.value = new Set() }
const checkedCount = computed(() => checkedPlanIds.value.size)
// A plan can be deleted unless one of its jobs is running: deleting the
// workspace out from under a live subprocess would corrupt the run.
const runningPlanIds = computed(() => new Set(jobs.value.filter(item => item.status === 'running' || item.status === 'queued').map(item => item.plan_id).filter((id): id is string => Boolean(id))))
const selectablePlans = computed(() => plans.value.filter(plan => !runningPlanIds.value.has(plan.id)))
const canDeleteChecked = computed(() => checkedCount.value > 0 && !isDeleting.value)

function toggleCheckAll(checked: boolean) {
  checkedPlanIds.value = checked ? new Set(selectablePlans.value.map(plan => plan.id)) : new Set()
}

async function deleteCheckedPlans() {
  const ids = [...checkedPlanIds.value]
  if (!ids.length || isDeleting.value) return
  const names = plans.value.filter(plan => checkedPlanIds.value.has(plan.id)).map(plan => plan.name)
  const confirmed = window.confirm(
    `确定删除以下 ${ids.length} 个计划吗？\n\n${names.join('\n')}\n\n` +
    '这会同时删除它们的成果包、任务临时产物和已上传的影像，且无法恢复。'
  )
  if (!confirmed) return
  await deletePlans(ids)
}

async function deletePlans(ids: string[]) {
  isDeleting.value = true; errorMessage.value = ''; deleteNotice.value = ''
  try {
    const result = await request<{ deleted: { name: string; freed_bytes: number }[]; failed: Record<string, string>; succeeded: number }>(
      '/api/plans', { method: 'DELETE', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ids }) }
    )
    const freed = result.deleted.reduce((sum, item) => sum + (item.freed_bytes || 0), 0)
    const failedNames = Object.keys(result.failed ?? {})
    if (failedNames.length) {
      deleteNotice.value = `已删除 ${result.succeeded} 个，释放 ${formatBytes(freed)}；${failedNames.length} 个失败：${Object.values(result.failed).join('；')}`
    } else {
      deleteNotice.value = `已删除 ${result.succeeded} 个计划，释放 ${formatBytes(freed)}`
    }
    clearCheckedPlans()
    if (ids.includes(selectedPlanId.value)) { selectedPlanId.value = ''; job.value = null; closeEvents() }
    await refresh()
  } catch (error) {
    errorMessage.value = error instanceof Error ? error.message : '删除计划失败'
  } finally { isDeleting.value = false }
}
async function cancelJob() { if (!job.value) return; try { job.value = await request<Job>(`/api/jobs/${encodeURIComponent(job.value.id)}/cancel`, { method: 'DELETE' }); closeEvents(); await refresh() } catch (error) { errorMessage.value = error instanceof Error ? error.message : '停止失败' } }
async function resumeJob() {
  if (!job.value?.resumable) return
  try { selectJob(await request<Job>(`/api/jobs/${encodeURIComponent(job.value.id)}/resume`, { method: 'POST' })) }
  catch (error) { errorMessage.value = error instanceof Error ? error.message : '恢复任务失败' }
}
function selectJob(next: Job) { job.value = next; connectEvents(next.id) }
function connectEvents(id: string) {
  closeEvents(); eventSource = new EventSource(`/api/jobs/${encodeURIComponent(id)}/events`)
  const update = (event: Event) => { try { job.value = JSON.parse((event as MessageEvent<string>).data) as Job; if (job.value.status === 'completed' || job.value.status === 'failed') { closeEvents(); refresh() } } catch { errorMessage.value = '实时日志数据异常' } }
  eventSource.addEventListener('snapshot', update); eventSource.addEventListener('update', update)
}
function closeEvents() { eventSource?.close(); eventSource = null }
function download() { if (job.value?.status === 'completed') window.location.assign(`/api/jobs/${encodeURIComponent(job.value.id)}/download`) }
async function copyInstallCommand() {
  const command = config.value?.deployment?.tool_install_command
  if (!command) return
  try {
    await navigator.clipboard.writeText(command)
    installCommandCopied.value = true
    window.setTimeout(() => { installCommandCopied.value = false }, 1800)
  } catch { errorMessage.value = '无法复制命令，请手动选择复制' }
}
onMounted(refresh); onBeforeUnmount(closeEvents)
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
