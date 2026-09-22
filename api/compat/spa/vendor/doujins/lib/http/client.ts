// The ONE frontend HTTP client (#763): auth-header injection, single-flight
// 401 refresh + retry, error normalization, and a lang-header/query default.
// ApiService, the react-query hooks (useApi.ts), and the billing clients all
// build on this instead of each reimplementing auth + refresh.
import { useAuthStore } from '@/store'
import { authSDK } from '@/services/auth/sdk'

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

export class HttpError extends Error {
  status?: number
  data?: unknown
  // The response JSON as sent; `data` is the flattened error envelope.
  body?: unknown
  constructor(
    message: string,
    status?: number,
    data?: unknown,
    body?: unknown
  ) {
    super(message)
    this.name = 'HttpError'
    this.status = status
    this.data = data
    this.body = body
  }
}

export type HttpRequestOptions = {
  params?: Record<string, string | number>
  query?: Record<string, string | number | boolean | undefined>
  body?: unknown
  headers?: Record<string, string>
  keepalive?: boolean
  signal?: AbortSignal
  retryAuth?: boolean
  omitAuth?: boolean
}

export type HttpStreamOptions<T> = Omit<HttpRequestOptions, 'body'> & {
  delimiter?: RegExp | string
  parse?: (chunk: string) => T
}

export interface HttpClientConfig {
  baseUrl: string
  // Inject an Authorization header from the auth store / AuthSDK. Off for the
  // auth-service client, whose callers set Authorization explicitly per-call.
  auth?: boolean
}

// Single-flight refresh lives in AuthSDK itself (concurrent callers join the
// same in-flight promise); this just calls into it so every client stack
// shares the one refresh path instead of reimplementing it.
const ensureRefreshed = async (): Promise<void> => {
  try {
    const success = await authSDK.refreshTokens(true)
    if (!success) throw { hard: false }
  } catch {
    throw { hard: false }
  }
}

const getCurrentLanguage = (): string => {
  try {
    let lang = window.lang
    if (!lang) {
      const m = window.location.pathname.match(/^\/([a-z]{2})(?:\/|$)/)
      if (m) lang = m[1]
    }
    return lang || 'en'
  } catch {
    return 'en'
  }
}

const getAuthHeaderValue = (): string | undefined => {
  const user = useAuthStore.getState().user
  if (user?.isAuthenticated && user?.token) return `Bearer ${user.token}`

  const tokenFromSDK = authSDK.getAccessToken()
  if (tokenFromSDK) return `Bearer ${tokenFromSDK}`
  return undefined
}

const normalizeErrorData = (errorData: any) => {
  const apiErrField = errorData?.error
  if (!apiErrField || typeof apiErrField !== 'object') return errorData
  return {
    ...errorData,
    ...(apiErrField.metadata && typeof apiErrField.metadata === 'object'
      ? apiErrField.metadata
      : {}),
    error:
      typeof apiErrField.code === 'string'
        ? apiErrField.code
        : typeof apiErrField.message === 'string'
          ? apiErrField.message
          : errorData?.error,
  }
}

const errorMessageFromData = (errorData: any, fallback: string) => {
  const apiErrField = errorData?.error
  if (typeof apiErrField === 'string') return apiErrField
  if (apiErrField && typeof apiErrField === 'object') {
    return (
      (typeof apiErrField.code === 'string' && apiErrField.code) ||
      (typeof apiErrField.message === 'string' && apiErrField.message)
    )
  }
  return errorData?.message || fallback
}

const buildError = async (response: Response): Promise<HttpError> => {
  const error = new HttpError(
    `HTTP error! status: ${response.status}`,
    response.status
  )
  try {
    const errorData = await response.json()
    error.message = errorMessageFromData(errorData, error.message)
    error.data = normalizeErrorData(errorData)
    error.body = errorData
  } catch {
    error.message = response.statusText || error.message
  }
  return error
}

const formatUrl = (
  apiPath: string,
  path: string,
  options?: HttpRequestOptions
): string => {
  let url = path

  if (options?.params) {
    for (const [key, value] of Object.entries(options.params)) {
      url = url.replace(`:${key}`, String(value))
    }
  }

  const fragmentParts = url.split('#')
  const urlWithoutFragment = fragmentParts[0]
  const fragment = fragmentParts.length > 1 ? fragmentParts[1] : ''

  const urlParts = urlWithoutFragment.split('?')
  const basePath = urlParts[0]
  const existingQuery = urlParts.length > 1 ? urlParts[1] : ''

  const existingParams: Record<string, string> = {}
  if (existingQuery) {
    new URLSearchParams(existingQuery).forEach((value, key) => {
      existingParams[key] = value
    })
  }

  const queryParams: Record<string, string | number | boolean | undefined> = {
    ...existingParams,
    ...options?.query,
  }
  if (!queryParams.lang) queryParams.lang = getCurrentLanguage()

  const searchParams = new URLSearchParams()
  for (const [key, value] of Object.entries(queryParams)) {
    if (value !== undefined) searchParams.set(key, String(value))
  }
  const queryString = searchParams.toString()
  url = `${basePath}${queryString ? `?${queryString}` : ''}${fragment ? `#${fragment}` : ''}`

  return `${apiPath}${url}`
}

export const createHttpClient = (config: HttpClientConfig) => {
  const apiPath = config.baseUrl
  const auth = config.auth ?? true

  const buildHeaders = (
    override?: Record<string, string>,
    isFormData?: boolean,
    omitAuth?: boolean
  ): Record<string, string> => {
    const headers: Record<string, string> = isFormData
      ? {}
      : { 'Content-Type': 'application/json' }
    if (auth && !omitAuth) {
      const authHeader = getAuthHeaderValue()
      if (authHeader) headers.Authorization = authHeader
    }
    headers['Accept-Language'] = getCurrentLanguage()
    return { ...headers, ...override }
  }

  const doFetch = (
    url: string,
    method: string,
    headers: Record<string, string>,
    options?: HttpRequestOptions
  ): Promise<Response> => {
    const isFormData = options?.body instanceof FormData
    return fetch(url, {
      method,
      headers,
      body: isFormData
        ? (options?.body as FormData)
        : options?.body !== undefined
          ? JSON.stringify(options.body)
          : undefined,
      mode: 'cors',
      credentials: 'include',
      keepalive: options?.keepalive,
      signal: options?.signal,
    })
  }

  // Shared 401-retry-once flow for the JSON request path and getRaw/stream.
  // A 401 response is never treated as a session verdict here: only the
  // SDK's terminal refresh failure ends the session (it clears auth state
  // and notifies expiry listeners itself, inside ensureRefreshed's call).
  // Endpoints can 401 for their own reasons after a successful refresh —
  // clearing anything on that used to log the user out of the whole app.
  const withAuthRetry = async (
    url: string,
    method: string,
    options: HttpRequestOptions | undefined,
    isFormData: boolean,
    initial: Response
  ): Promise<Response> => {
    if (options?.retryAuth === false || initial.status !== 401 || !auth) {
      return initial
    }
    try {
      await ensureRefreshed()
      const retryHeaders = buildHeaders(
        options?.headers,
        isFormData,
        options?.omitAuth
      )
      return await doFetch(url, method, retryHeaders, options)
    } catch {
      return initial
    }
  }

  const request = async <T>(
    method: HttpMethod,
    path: string,
    options?: HttpRequestOptions
  ): Promise<T> => {
    const url = formatUrl(apiPath, path, options)
    const isFormData = options?.body instanceof FormData
    const headers = buildHeaders(
      options?.headers,
      isFormData,
      options?.omitAuth
    )

    const initial = await doFetch(url, method, headers, options)
    const response = await withAuthRetry(
      url,
      method,
      options,
      isFormData,
      initial
    )

    if (!response.ok) {
      const error = await buildError(response)
      throw error
    }

    // 204 acks and 202 anti-enumeration sends (authkit #313) carry no body.
    if (response.status === 204) return undefined as T
    const text = await response.text()
    if (text.trim() === '') return undefined as T
    return JSON.parse(text) as T
  }

  const getRaw = async (
    path: string,
    options?: Omit<HttpRequestOptions, 'body'>
  ): Promise<Response> => {
    const url = formatUrl(apiPath, path, options)
    const headers = buildHeaders(options?.headers, false, options?.omitAuth)
    delete headers['Content-Type']

    const initial = await doFetch(url, 'GET', headers, options)
    const response = await withAuthRetry(url, 'GET', options, false, initial)

    if (!response.ok) {
      const error = await buildError(response)
      throw error
    }
    return response
  }

  const stream = async <T = unknown>(
    path: string,
    onMessage: (data: T) => void,
    options?: HttpStreamOptions<T>
  ): Promise<void> => {
    const url = formatUrl(apiPath, path, options)
    const headers = buildHeaders(options?.headers, false, options?.omitAuth)

    const initial = await doFetch(url, 'GET', headers, options)
    const response = await withAuthRetry(url, 'GET', options, false, initial)

    if (!response.ok) {
      const error = await buildError(response)
      throw error
    }

    const reader = response.body?.getReader()
    if (!reader) throw new Error('Response body reader not available')

    const decoder = new TextDecoder()
    const splitPattern = options?.delimiter ?? /\r\n|\n|\r/
    const parseChunk =
      options?.parse ?? ((line: string) => JSON.parse(line) as T)
    let buffer = ''

    try {
      while (true) {
        const { value, done } = await reader.read()
        if (done) break

        buffer += decoder.decode(value, { stream: true })
        const lines = buffer.split(splitPattern)
        buffer = lines.pop() || ''

        for (const line of lines) {
          if (line.trim()) {
            try {
              onMessage(parseChunk(line))
            } catch (parseError) {
              console.error('Failed to parse stream message:', parseError, line)
            }
          }
        }
      }

      const finalChunk = buffer + decoder.decode()
      if (finalChunk.trim()) {
        try {
          onMessage(parseChunk(finalChunk))
        } catch (parseError) {
          console.error(
            'Failed to parse final stream message:',
            parseError,
            finalChunk
          )
        }
      }
    } finally {
      reader.releaseLock()
    }
  }

  return {
    request,
    get: <T>(path: string, options?: Omit<HttpRequestOptions, 'body'>) =>
      request<T>('GET', path, options),
    post: <T>(path: string, options?: HttpRequestOptions) =>
      request<T>('POST', path, options),
    put: <T>(path: string, options?: HttpRequestOptions) =>
      request<T>('PUT', path, options),
    patch: <T>(path: string, options?: HttpRequestOptions) =>
      request<T>('PATCH', path, options),
    delete: <T>(path: string, options?: HttpRequestOptions) =>
      request<T>('DELETE', path, options),
    getRaw,
    stream,
  }
}

export type HttpClient = ReturnType<typeof createHttpClient>
