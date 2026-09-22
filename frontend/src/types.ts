// types.ts holds the shapes exchanged with the Go service. They mirror the JSON
// tags in cmd/server/types.go and api.go; keep the two in step when the wire
// format changes.

export type LogEntry = { at: string; stream: string; message: string }

export type Job = {
  id: string
  status: 'queued' | 'running' | 'completed' | 'failed'
  phase: string
  progress: number
  created_at: string
  updated_at: string
  input_path: string
  output_path: string
  input_count: number
  input_bytes: number
  plan_id?: string
  source_type?: string
  error?: string
  logs: LogEntry[]
  checkpoint?: string
  resumable?: boolean
  stats?: {
    vertices: number
    faces: number
    tiles?: number
    valid_tiles?: number
    failed_tiles?: number
    textured?: boolean
  }
}

export type Plan = {
  id: string
  name: string
  upload_id?: string
  input_path?: string
  status: string
  scheduled_at?: string
  created_at: string
  updated_at: string
  last_job_id?: string
}

export type Upload = { id: string; path: string; files: number; bytes: number; created_at: string }

export type ToolPath = {
  key: string
  label: string
  value: string
  available: boolean
  required: boolean
}

export type Deployment = {
  kind: string
  tool_install_mode: string
  tool_install_command: string
  automatic: boolean
  can_execute_here: boolean
  note: string
}

export type ServiceConfig = {
  native_pipeline_ready: boolean
  dependencies: Record<string, boolean>
  pipeline_mode: string
  output_root: string
  config_file: string
  native_directory_picker: boolean
  tool_paths: ToolPath[]
  deployment?: Deployment
}

// Outcome of a plan deletion, as returned by DELETE /api/plans.
export type DeleteResult = {
  deleted: {
    plan_id: string
    name: string
    jobs_deleted: string[]
    upload_deleted?: string
    upload_retained?: string
    deliverables_deleted: boolean
    freed_bytes: number
    warnings?: string[]
  }[]
  failed: Record<string, string>
  total: number
  succeeded: number
}

// The pipeline stages shown on the timeline, in order.
export const PHASES = [
  ['preparing', '影像预处理'],
  ['features', '特征提取'],
  ['matching', '特征匹配'],
  ['sfm', '空三 / 束调'],
  ['dense', '稠密点云'],
  ['mesh', '网格重建'],
  ['texture', '纹理映射'],
  ['georeference', '坐标检查'],
  ['lod', 'Smart3D 分层 OSGB'],
  ['osgb_conversion', 'OSGB 转换与校验'],
  ['completed', '完成'],
] as const
