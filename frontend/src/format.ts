// format.ts holds display-only helpers shared by the composables and the view.

import { PHASES } from './types'

// formatBytes renders a byte count as MB below 1 GB and GB above it, which is
// the range survey image sets and deliverables actually fall in.
export function formatBytes(bytes = 0): string {
  return bytes < 1024 ** 3
    ? `${(bytes / 1024 ** 2).toFixed(1)} MB`
    : `${(bytes / 1024 ** 3).toFixed(2)} GB`
}

const STATUS_LABELS: Record<string, string> = {
  queued: '排队中',
  running: '处理中',
  completed: '已完成',
  failed: '失败',
  scheduled: '已预约',
  draft: '草稿',
}

export function statusText(status?: string): string {
  return STATUS_LABELS[status ?? ''] ?? '待处理'
}

// phaseIndex maps a phase key to its position on the timeline. An unknown phase
// reports the first step so the timeline never renders empty.
export function phaseIndex(phase?: string): number {
  const index = PHASES.findIndex(([key]) => key === phase)
  return index < 0 ? 0 : index
}

// phaseLabel resolves a phase key to its Chinese label, falling back to the raw
// key so an unexpected value is still visible rather than blank.
export function phaseLabel(phase?: string): string {
  return PHASES[phaseIndex(phase)]?.[1] ?? phase ?? '尚未开始'
}

export { PHASES }
