// API Error Types - matches backend types.go
export interface APIError {
  message: string
  type: string
  code: number | string
  error_subcode?: number
  is_transient: boolean // Tell frontend to retry or not
  error_user_title?: string
  error_user_msg?: string
}

export interface APISuccess {
  message: string
  type: string
  code: number
  success_user_title?: string
  success_user_msg?: string
}

// Error type constants that match backend
export const API_ERROR_TYPES = {
  VIDEO_NOT_FOUND: 'video_not_found',
  HLS_CONTENT_NOT_FOUND: 'hls_content_not_found',
  BUCKET_ERROR: 'bucket_error',
  INVALID_QUALITY: 'invalid_quality',
  INVALID_INPUT: 'invalid_input',
  UNAUTHORIZED: 'unauthorized',
  // ak#290: every server-side failure is `internal_error` on the wire.
  INTERNAL_ERROR: 'internal_error',
} as const

export type APIErrorType =
  (typeof API_ERROR_TYPES)[keyof typeof API_ERROR_TYPES]

// Helper to check if error is of specific type
export const isErrorOfType = (error: unknown, type: APIErrorType): boolean =>
  typeof error === 'object' && error !== null && 'type' in error && error.type === type

// Helper to get user-friendly error message
export const getUserErrorMessage = (error: APIError): string => {
  if (error.error_user_msg) {
    return error.error_user_msg
  }

  // Fallback messages for different error types
  switch (error.type) {
    case API_ERROR_TYPES.VIDEO_NOT_FOUND:
      return 'The video you requested could not be found.'
    case API_ERROR_TYPES.HLS_CONTENT_NOT_FOUND:
      return 'Video streaming content is not available. Please try again later.'
    case API_ERROR_TYPES.BUCKET_ERROR:
      return 'There was an issue accessing the video content. Please try again later.'
    case API_ERROR_TYPES.INVALID_QUALITY:
      return 'The requested video quality is not supported.'
    case API_ERROR_TYPES.UNAUTHORIZED:
      return 'You are not authorized to access this content.'
    case API_ERROR_TYPES.INTERNAL_ERROR:
      return 'A server error occurred. Please try again later.'
    default:
      return error.message || 'An unexpected error occurred.'
  }
}
