// api.ts is the single place that talks HTTP. Keeping it separate means the
// composables contain only state and intent, and the error-message convention
// (the service returns {code, detail}) is handled once.

import type {
  DeleteResult, Job, Plan, ServiceConfig, Upload,
} from './types'

// ApiError carries the service's error code alongside its message so callers can
// branch on the code instead of matching message text.
export class ApiError extends Error {
  code: string

  constructor(message: string, code = '') {
    super(message)
    this.name = 'ApiError'
    this.code = code
  }
}

export async function request<T>(url: string, options?: RequestInit): Promise<T> {
  const response = await fetch(url, options)
  let data: unknown = null
  try {
    data = await response.json()
  } catch {
    // A non-JSON body (a proxy error page, for instance) is reported below
    // rather than surfacing as a parse failure.
  }
  // 207 Multi-Status is a success for our purposes: a batch delete reports which
  // entries were removed and which were refused in the body, and the caller
  // needs that detail. Treating it as an error discarded the per-plan results
  // and showed a bare "request failed" instead.
  const ok = response.ok || response.status === 207
  if (!ok) {
    const payload = data as { detail?: string; code?: string } | null
    throw new ApiError(payload?.detail || '请求失败', payload?.code || '')
  }
  return data as T
}

export const api = {
  config: () => request<ServiceConfig>('/api/config'),

  saveTools: (tools: Record<string, string>) => request<ServiceConfig>('/api/config', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ tools }),
  }),

  uploadImages: (files: File[]) => {
    const form = new FormData()
    for (const file of files) form.append('files', file, file.webkitRelativePath || file.name)
    return request<Upload>('/api/uploads', { method: 'POST', body: form })
  },

  plans: () => request<{ plans: Plan[] }>('/api/plans'),
  jobs: () => request<{ jobs: Job[] }>('/api/jobs'),

  createPlan: (body: Record<string, unknown>) => request<Plan | { plan: Plan; job: Job }>('/api/plans', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }),

  runPlan: (planID: string) => request<Job>(`/api/plans/${encodeURIComponent(planID)}/run`, { method: 'POST' }),

  deletePlans: (ids: string[]) => request<DeleteResult>('/api/plans', {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  }),

  cancelJob: (jobID: string) => request<Job>(`/api/jobs/${encodeURIComponent(jobID)}/cancel`, { method: 'DELETE' }),
  resumeJob: (jobID: string) => request<Job>(`/api/jobs/${encodeURIComponent(jobID)}/resume`, { method: 'POST' }),

  eventsURL: (jobID: string) => `/api/jobs/${encodeURIComponent(jobID)}/events`,
  downloadURL: (jobID: string) => `/api/jobs/${encodeURIComponent(jobID)}/download`,
}

// errorText normalises anything thrown into a displayable message.
export function errorText(error: unknown, fallback: string): string {
  if (error instanceof Error && error.message) return error.message
  return fallback
}
