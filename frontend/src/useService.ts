// useService.ts owns everything about the service itself rather than about work
// being done: the dependency/tool configuration, the tool-settings dialog, and
// image upload.

import { computed, ref } from 'vue'
import { api, errorText } from './api'
import type { ServiceConfig, Upload } from './types'

export function useService(onError: (message: string) => void) {
  const config = ref<ServiceConfig | null>(null)
  const toolDraft = ref<Record<string, string>>({})
  const showToolSettings = ref(false)
  const isSavingTools = ref(false)
  const installCommandCopied = ref(false)

  const selectedFiles = ref<File[]>([])
  const upload = ref<Upload | null>(null)
  const isUploading = ref(false)

  const ready = computed(() => Boolean(config.value?.native_pipeline_ready))
  const canUpload = computed(() => selectedFiles.value.length > 0 && !isUploading.value)

  function applyConfig(next: ServiceConfig) {
    config.value = next
    toolDraft.value = Object.fromEntries(next.tool_paths.map(tool => [tool.key, tool.value]))
  }

  async function refreshService() {
    applyConfig(await api.config())
  }

  async function saveToolPaths() {
    if (!config.value) return
    isSavingTools.value = true
    try {
      applyConfig(await api.saveTools(toolDraft.value))
      showToolSettings.value = false
    } catch (error) {
      onError(errorText(error, '工具配置保存失败'))
    } finally {
      isSavingTools.value = false
    }
  }

  function chooseFiles(event: Event) {
    selectedFiles.value = Array.from((event.target as HTMLInputElement).files ?? [])
    upload.value = null
  }

  async function uploadFiles() {
    if (!canUpload.value) return
    isUploading.value = true
    try {
      upload.value = await api.uploadImages(selectedFiles.value)
    } catch (error) {
      onError(errorText(error, '上传失败'))
    } finally {
      isUploading.value = false
    }
  }

  async function copyInstallCommand() {
    const command = config.value?.deployment?.tool_install_command
    if (!command) return
    try {
      await navigator.clipboard.writeText(command)
      installCommandCopied.value = true
      window.setTimeout(() => { installCommandCopied.value = false }, 1800)
    } catch {
      onError('无法复制命令，请手动选择复制')
    }
  }

  return {
    config, toolDraft, showToolSettings, isSavingTools, installCommandCopied,
    selectedFiles, upload, isUploading,
    ready, canUpload,
    applyConfig, refreshService, saveToolPaths, chooseFiles, uploadFiles, copyInstallCommand,
  }
}
