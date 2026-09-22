// useJobs.ts owns the job list and the live monitor: which job is selected, its
// server-sent event stream, and the actions that operate on it.

import { computed, onBeforeUnmount, ref } from 'vue'
import { api, errorText } from './api'
import type { Job } from './types'

export function useJobs(onError: (message: string) => void) {
  const jobs = ref<Job[]>([])
  const job = ref<Job | null>(null)
  let eventSource: EventSource | null = null

  const isRunning = computed(() => job.value?.status === 'queued' || job.value?.status === 'running')

  // Plans whose jobs are still active. Used by the plan list to disable
  // deletion, because removing a workspace underneath a live subprocess would
  // break the run.
  const runningPlanIds = computed(() => new Set(
    jobs.value
      .filter(item => item.status === 'running' || item.status === 'queued')
      .map(item => item.plan_id)
      .filter((id): id is string => Boolean(id)),
  ))

  function closeEvents() {
    eventSource?.close()
    eventSource = null
  }

  function connectEvents(id: string) {
    closeEvents()
    eventSource = new EventSource(api.eventsURL(id))
    const update = (event: Event) => {
      try {
        job.value = JSON.parse((event as MessageEvent<string>).data) as Job
        // The stream is only useful while the job is live; refreshing on a
        // terminal state also picks up the plan list's new status.
        if (job.value.status === 'completed' || job.value.status === 'failed') {
          closeEvents()
          void refreshJobs()
        }
      } catch {
        onError('实时日志数据异常')
      }
    }
    eventSource.addEventListener('snapshot', update)
    eventSource.addEventListener('update', update)
  }

  function selectJob(next: Job) {
    job.value = next
    connectEvents(next.id)
  }

  async function refreshJobs() {
    const response = await api.jobs()
    jobs.value = response.jobs
  }

  async function cancelJob() {
    if (!job.value) return
    try {
      job.value = await api.cancelJob(job.value.id)
      closeEvents()
    } catch (error) {
      onError(errorText(error, '停止失败'))
    }
  }

  async function resumeJob() {
    if (!job.value?.resumable) return
    try {
      selectJob(await api.resumeJob(job.value.id))
    } catch (error) {
      onError(errorText(error, '恢复任务失败'))
    }
  }

  function download() {
    if (job.value?.status === 'completed') window.location.assign(api.downloadURL(job.value.id))
  }

  // Dropping the selected job is needed when a plan is deleted out from under it.
  function clearSelection() {
    closeEvents()
    job.value = null
  }

  onBeforeUnmount(closeEvents)

  return {
    jobs, job, isRunning, runningPlanIds,
    refreshJobs, selectJob, cancelJob, resumeJob, download, clearSelection, closeEvents,
  }
}
