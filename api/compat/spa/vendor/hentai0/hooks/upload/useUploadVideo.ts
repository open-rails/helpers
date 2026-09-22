import axios, { isAxiosError } from 'axios'
import { useState } from 'react'
import { getToken } from '@/hooks/auth/actions'

interface VideoUploadErrorResponse {
  object: 'error'
  error: {
    type: string
    message: string
    code: number
    is_transient: boolean
    error_user_title?: string
    error_user_msg?: string
  }
  results?: Array<{ staging_key: string; error?: string }>
}

interface PresignVideoUploadResponse {
  upload_url: string
  fields?: Record<string, string>
  staging_key: string
  expires_at: string
}

// Acceptance only; processing is reported by the version's media status. A
// retried commit answers with the same version and replayed=true.
interface CommitVideoUploadResult {
  staging_key: string
  video_id?: string
  version_id?: string
  request_id?: string
  replayed?: boolean
  error?: string
}

interface UploadProgressOptions {
  fileIndex?: number
  totalFiles?: number
  onProgress?: (progress: number) => void
}

export const useUploadVideo = () => {
  const [uploading, setUploading] = useState(false)
  const [progress, setProgress] = useState<number>(0)

  const presignUpload = async (file: File, uploadId?: string) => {
    const response = await axios.post<PresignVideoUploadResponse>(
      `/api/v1/admin/uploads/presign`,
      {
        upload_id: uploadId || crypto.randomUUID(),
        file_name: file.name,
        file_size: file.size,
        mime_type: file.type || 'application/octet-stream',
      },
      {
        headers: {
          Authorization: `Bearer ${getToken()}`,
        },
      }
    )

    return response.data
  }

  const uploadToStaging = async (
    file: File,
    presign: PresignVideoUploadResponse,
    options: UploadProgressOptions = {}
  ) => {
    const handleProgress = (progressEvent: { loaded: number; total?: number }) => {
      if (!progressEvent.total) return
      const fileProgress = progressEvent.loaded / progressEvent.total
      const percent = Math.round(fileProgress * 100)
      if (options.onProgress) {
        options.onProgress(percent)
      }
      if (
        typeof options.fileIndex === 'number' &&
        typeof options.totalFiles === 'number'
      ) {
        const overall = Math.round(
          ((options.fileIndex + fileProgress) / options.totalFiles) * 100
        )
        setProgress(overall)
      }
    }

    // New flow: S3/MinIO presigned POST form upload.
    if (presign.fields && Object.keys(presign.fields).length > 0) {
      const formData = new FormData()
      for (const [key, value] of Object.entries(presign.fields)) {
        formData.append(key, value)
      }
      formData.append('file', file, file.name)

      await axios.post(presign.upload_url, formData, {
        onUploadProgress: handleProgress,
      })
      return
    }

    // Backward-compatible fallback: presigned PUT URL.
    await axios.put(presign.upload_url, file, {
      headers: {
        'Content-Type': file.type || 'application/octet-stream',
      },
      onUploadProgress: (progressEvent) => {
        handleProgress(progressEvent)
      },
    })
  }

  const commitUploads = async (
    items: Array<{ staging_key: string; metadata: Record<string, unknown> }>
  ) => {
    const response = await axios.post<{ results: CommitVideoUploadResult[] }>(
      `/api/v1/admin/uploads/commit`,
      { items },
      {
        headers: {
          Authorization: `Bearer ${getToken()}`,
        }
      },
    )

    return response.data
  }

  const getUploadErrorMessage = (error: unknown) => {
    let errorMessage = 'Upload failed - please try again'

    if (isAxiosError<VideoUploadErrorResponse>(error) && error.response?.data) {
      const errorData = error.response.data
      if (errorData.object === 'error' && errorData.error) {
        errorMessage = errorData.error.error_user_msg || errorData.error.message
      }

      if (errorData.results && errorData.results.length > 0) {
        const first = errorData.results.find((item) => item.error)
        if (first?.error) {
          errorMessage = first.error
        }
      }
    }

    return errorMessage
  }

  const uploadQueue = async (
    items: Array<{ id: string; file: File }>,
    callbacks: {
      onItemProgress?: (id: string, progress: number) => void
      onItemUploaded?: (id: string, stagingKey: string) => void | Promise<void>
      onItemError?: (id: string, errorMessage: string) => void
    } = {}
  ) => {
    if (items.length === 0) {
      return [] as Array<{ id: string; stagingKey: string }>
    }

    setUploading(true)
    setProgress(0)

    const results: Array<{ id: string; stagingKey: string }> = []

    try {
      for (let i = 0; i < items.length; i += 1) {
        const item = items[i]
        try {
          const presign = await presignUpload(item.file, item.id)
          await uploadToStaging(item.file, presign, {
            fileIndex: i,
            totalFiles: items.length,
            onProgress: (progressValue) => {
              callbacks.onItemProgress?.(item.id, progressValue)
            },
          })
          await callbacks.onItemUploaded?.(item.id, presign.staging_key)
          results.push({ id: item.id, stagingKey: presign.staging_key })
          setProgress(Math.round(((i + 1) / items.length) * 100))
        } catch (error) {
          const errorMessage = getUploadErrorMessage(error)
          callbacks.onItemError?.(item.id, errorMessage)
        }
      }

      return results
    } finally {
      setUploading(false)
    }
  }

  return {
    uploading,
    progress,
    uploadQueue,
    presignUpload,
    uploadToStaging,
    commitUploads,
  }
}
