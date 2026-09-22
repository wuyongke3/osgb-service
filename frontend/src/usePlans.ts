// usePlans.ts owns the production plans: creating them, choosing data sources,
// selecting one (without starting it), and deleting them together with the
// artifacts they produced.

import { computed, ref } from 'vue'
import { api, errorText } from './api'
import type { Job, Plan, Upload } from './types'

type Hooks = {
  onError: (message: string) => void
  onJobStarted: (job: Job) => void
  onPlansDeleted: (ids: string[]) => void
  runningPlanIds: () => Set<string>
  formatBytes: (bytes?: number) => string
}

export function usePlans(hooks: Hooks) {
  const plans = ref<Plan[]>([])
  const planName = ref('')
  const scheduledAt = ref('')
  const sourceMode = ref<'upload' | 'path'>('upload')
  const inputPath = ref('')
  const upload = ref<Upload | null>(null)
  const isCreating = ref(false)

  // Selection for deletion, kept as a Set of ids so it survives a list refresh.
  const checkedPlanIds = ref<Set<string>>(new Set())
  const selectedPlanId = ref('')
  const isDeleting = ref(false)
  const deleteNotice = ref('')

  const checkedCount = computed(() => checkedPlanIds.value.size)

  // A plan whose job is running cannot be deleted, so it is never selectable.
  const selectablePlans = computed(() => plans.value.filter(plan => !hooks.runningPlanIds().has(plan.id)))
  const allSelected = computed(() => selectablePlans.value.length > 0 && checkedCount.value === selectablePlans.value.length)
  const someSelected = computed(() => checkedCount.value > 0 && checkedCount.value < selectablePlans.value.length)
  const canDeleteChecked = computed(() => checkedCount.value > 0 && !isDeleting.value)
  const canCreate = computed(() => Boolean(planName.value.trim())
    && (sourceMode.value === 'upload' ? Boolean(upload.value) : Boolean(inputPath.value.trim()))
    && !isCreating.value)

  // Selecting a plan only selects it. Starting is a deliberate second action
  // through runPlan, because a run occupies the pipeline for hours.
  function selectPlan(plan: Plan) {
    selectedPlanId.value = plan.id
    deleteNotice.value = ''
  }

  function togglePlanCheck(planId: string, checked: boolean) {
    const next = new Set(checkedPlanIds.value)
    if (checked) next.add(planId)
    else next.delete(planId)
    checkedPlanIds.value = next
  }

  function clearCheckedPlans() {
    checkedPlanIds.value = new Set()
  }

  function toggleCheckAll(checked: boolean) {
    checkedPlanIds.value = checked ? new Set(selectablePlans.value.map(plan => plan.id)) : new Set()
  }

  async function createPlan(startNow: boolean) {
    if (!canCreate.value) return
    isCreating.value = true
    try {
      const body: Record<string, unknown> = { name: planName.value.trim(), start_now: startNow }
      if (sourceMode.value === 'upload') body.upload_id = upload.value?.id
      else body.input_path = inputPath.value.trim()
      if (!startNow && scheduledAt.value) body.scheduled_at = new Date(scheduledAt.value).toISOString()

      const result = await api.createPlan(body)
      if ('job' in result) {
        hooks.onJobStarted(result.job)
        plans.value = [result.plan, ...plans.value.filter(item => item.id !== result.plan.id)]
      } else {
        plans.value = [result, ...plans.value]
      }
      planName.value = ''
      scheduledAt.value = ''
    } catch (error) {
      hooks.onError(errorText(error, '创建计划失败'))
    } finally {
      isCreating.value = false
    }
  }

  async function runPlan(plan: Plan) {
    try {
      hooks.onJobStarted(await api.runPlan(plan.id))
    } catch (error) {
      hooks.onError(errorText(error, '启动计划失败'))
    }
  }

  async function deleteCheckedPlans() {
    const ids = [...checkedPlanIds.value]
    if (!ids.length || isDeleting.value) return
    const names = plans.value.filter(plan => checkedPlanIds.value.has(plan.id)).map(plan => plan.name)
    const confirmed = window.confirm(
      `确定删除以下 ${ids.length} 个计划吗？\n\n${names.join('\n')}\n\n`
      + '这会同时删除它们的成果包、任务临时产物和已上传的影像，且无法恢复。',
    )
    if (!confirmed) return
    await deletePlans(ids)
  }

  async function deletePlans(ids: string[]) {
    isDeleting.value = true
    deleteNotice.value = ''
    try {
      const result = await api.deletePlans(ids)
      const freed = result.deleted.reduce((sum, item) => sum + (item.freed_bytes || 0), 0)
      const failures = Object.values(result.failed ?? {})
      deleteNotice.value = failures.length
        ? `已删除 ${result.succeeded} 个，释放 ${hooks.formatBytes(freed)}；${failures.length} 个失败：${failures.join('；')}`
        : `已删除 ${result.succeeded} 个计划，释放 ${hooks.formatBytes(freed)}`
      clearCheckedPlans()
      hooks.onPlansDeleted(ids)
    } catch (error) {
      hooks.onError(errorText(error, '删除计划失败'))
    } finally {
      isDeleting.value = false
    }
  }

  return {
    plans, planName, scheduledAt, sourceMode, inputPath, upload, isCreating,
    checkedPlanIds, selectedPlanId, isDeleting, deleteNotice,
    checkedCount, selectablePlans, allSelected, someSelected, canDeleteChecked, canCreate,
    selectPlan, togglePlanCheck, clearCheckedPlans, toggleCheckAll,
    createPlan, runPlan, deleteCheckedPlans, deletePlans,
  }
}
