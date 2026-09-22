import { getToken } from '@/hooks/auth/actions'
import { handleAuthFailureRedirect } from '@/hooks/auth/actions'
import { authSDK } from '@/services/auth/sdk'
import { APIError, getUserErrorMessage } from '@/types/api'
import { extractLanguageFromPath } from '@/utils/languages'
import {
  getAuthFailureRedirectPath,
  isAdminPath,
  isAuthPublicPath,
} from '@/utils/auth-route-guards'

type RequestOptions = {
  signal?: AbortSignal
  params?: Record<string, string | number>
  body?: unknown
  headers?: Record<string, string>
  query?: Record<string, string>
}

const abortedRequest = () =>
  new DOMException('Session or request changed', 'AbortError')

export class ApiService {
  private apiPath: string
  private origin: string
  private baseRoute: string
  private defaultHeaders: Record<string, string>

  private handleUnauthorizedNavigation(status: number, requestPath: string) {
    const currentPath = window.location.pathname
    const isPublicPath = isAuthPublicPath(currentPath)
    const isAdminRequest =
      requestPath === '/admin' || requestPath.startsWith('/admin/')
    const isAdminScreen = isAdminPath(currentPath)

    if (status === 401 && !isPublicPath) {
      handleAuthFailureRedirect(getAuthFailureRedirectPath())
      return
    }

    if (status === 403 && (isAdminRequest || isAdminScreen)) {
      handleAuthFailureRedirect(getAuthFailureRedirectPath())
    }
  }

  constructor(baseUrlOverride?: string, baseRoute = '/api/v1') {
    // Get base URL (host only) from environment variable or runtime injection
    const baseUrl = baseUrlOverride || ''
    // Always append baseRoute to the base URL
    this.apiPath = baseUrl ? `${baseUrl}${baseRoute}` : baseRoute
    this.origin = baseUrl
    this.baseRoute = baseRoute
    this.defaultHeaders = {
      'Content-Type': 'application/json',
    }
  }

  // Get fresh authorization header
  private getAuthHeaders(): Record<string, string> {
    const token = getToken()
    const headers: Record<string, string> = { ...this.defaultHeaders }
    if (token) {
      headers.Authorization = `Bearer ${token}`
    }
    if (typeof window !== 'undefined') {
      try {
        headers['Accept-Language'] = extractLanguageFromPath(
          window.location.pathname
        )
        // track language globally for mutating calls
        window.lang = headers['Accept-Language']
      } catch {
        headers['Accept-Language'] = 'en'
      }
    } else {
      headers['Accept-Language'] = 'en'
    }
    return headers
  }

  public getBaseUrl(): string {
    return this.apiPath
  }

  /**
   * Resolves a URL the backend returned. Absolute URLs (presigned storage)
   * pass through; root-relative API paths already carry the API prefix and
   * resolve against the API origin; anything else is relative to the API base.
   */
  public resolveUrl(url: string): string {
    if (/^https?:\/\//i.test(url)) return url
    const path = url.startsWith('/') ? url : `/${url}`
    if (path === this.baseRoute || path.startsWith(`${this.baseRoute}/`)) {
      return `${this.origin}${path}`
    }
    return `${this.apiPath}${path}`
  }

  // Helper to format URL with path parameters
  private formatUrl(path: string, options?: RequestOptions): string {
    let url = path

    // Replace path parameters
    if (options?.params) {
      Object.entries(options.params).forEach(([key, value]) => {
        url = url.replace(`:${key}`, String(value))
      })
    }

    // Add query parameters
    if (options?.query) {
      const queryString = new URLSearchParams(options.query).toString()
      url = `${url}${queryString ? `?${queryString}` : ''}`
    }

    const normalizedPath = url.startsWith('/') ? url : `/${url}`

    if (!this.apiPath) {
      return normalizedPath
    }

    if (this.apiPath.endsWith('/') && normalizedPath.startsWith('/')) {
      return `${this.apiPath}${normalizedPath.slice(1)}`
    }

    return `${this.apiPath}${normalizedPath}`
  }

  // Generic request method with automatic token refresh
  private async request<T>(
    method: string,
    path: string,
    options?: RequestOptions,
    isRetry: boolean = false
  ): Promise<T> {
    const generation = authSDK.getTokenManager().getGeneration()
    const anonymous = !getToken()
    const assertCurrent = () => {
      if (options?.signal?.aborted) throw abortedRequest()
      if (generation === authSDK.getTokenManager().getGeneration()) return
      // A generation bump with no principal on either side (the cold-load
      // /token 401 clearing an empty session) cannot make a public response stale.
      if (anonymous && !authSDK.getAccessToken()) return
      throw abortedRequest()
    }
    const refresh = async () => {
      const refreshed = await authSDK.refreshTokens(true)
      if (authSDK.getAccessToken()) assertCurrent()
      return refreshed
    }
    assertCurrent()
    const url = this.formatUrl(path, options)
    const headers = { ...this.getAuthHeaders(), ...options?.headers }
    const isFormData = options?.body instanceof FormData
    if (isFormData) {
      // Let the browser set multipart boundary.
      delete headers['Content-Type']
    }

    // Add timeout to prevent hanging requests (30 seconds)
    const controller = new AbortController()
    const timeoutId = setTimeout(() => controller.abort(), 30000)

    try {
      const response = await fetch(`${url}`, {
        method,
        headers,
        body: options?.body
          ? isFormData
            ? (options.body as FormData)
            : JSON.stringify(options.body)
          : undefined,
        credentials: 'include', // Include cookies for refresh token
        signal: options?.signal
          ? AbortSignal.any([controller.signal, options.signal])
          : controller.signal,
      })
      clearTimeout(timeoutId)
      assertCurrent()

      // AuthSDK owns the single-flight cookie exchange across every client.
      if (response.status === 401 && !isRetry) {
        if (await refresh()) {
          return this.request<T>(method, path, options, true)
        }
      }

      if (!response.ok) {
        // If we get a 401 after refresh attempt, redirect to auth entry route
        if (
          typeof window !== 'undefined' &&
          !(response.status === 401 && !isRetry && authSDK.getAccessToken())
        ) {
          this.handleUnauthorizedNavigation(response.status, path)
        }

        // For other errors, try to parse the error response
        let errorData: unknown
        try {
          errorData = await response.json()
        } catch {
          throw new Error(`HTTP error! status: ${response.status}`)
        }

        if (errorData && typeof errorData === 'object') {
          const isPremiumRequired = (msg?: string) =>
            typeof msg === 'string' &&
            msg.toLowerCase().includes('premium membership required')

          // Check for Stripe-like error envelope: { error: { type, code, message } }
          const errObj = errorData as Record<string, unknown>
          if (errObj.error && typeof errObj.error === 'object') {
            const stripeError = errObj.error as Record<string, unknown>
            const enrichedError = {
              status: response.status,
              data: errorData,
              is_transient: stripeError.is_transient === true,
              type: stripeError.type as string,
              code: stripeError.code as string,
              message: stripeError.message as string,
              param: stripeError.param as string | undefined,
            } as APIError & { status: number; code: string; param?: string }

            if (
              response.status === 403 &&
              !isRetry &&
              isPremiumRequired(enrichedError.message)
            ) {
              if (await refresh()) {
                return this.request<T>(method, path, options, true)
              }
            }

            throw {
              ...enrichedError,
              userMessage:
                enrichedError.message || getUserErrorMessage(enrichedError),
            }
          }

          // Standard (house) error format: { error: message, code, metadata?, fields? }
          const enrichedError = {
            status: response.status,
            ...(errorData as Record<string, unknown>),
          } as APIError & { status: number }
          if (!enrichedError.message && typeof errObj.error === 'string') {
            enrichedError.message = errObj.error
          }

          if (enrichedError.type && enrichedError.message) {
            if (
              response.status === 403 &&
              !isRetry &&
              isPremiumRequired(enrichedError.message)
            ) {
              if (await refresh()) {
                return this.request<T>(method, path, options, true)
              }
            }

            throw {
              ...enrichedError,
              userMessage: getUserErrorMessage(enrichedError),
            }
          }

          throw enrichedError
        }

        throw {
          status: response.status,
          message:
            typeof errorData === 'string' && errorData
              ? errorData
              : 'Request failed',
        }
      }

      // ak#313: mutations with nothing to return answer 204, anti-enumeration
      // sends answer 202 with an empty body — neither carries JSON.
      if (
        response.status === 204 ||
        response.status === 202 ||
        response.headers.get('content-length') === '0'
      ) {
        const raw = await response.text()
        if (!raw.trim()) {
          return undefined as T
        }
        return JSON.parse(raw) as T
      }

      const data = await response.json()
      assertCurrent()

      if (
        data &&
        typeof data === 'object' &&
        (data as Record<string, unknown>).object === 'error'
      ) {
        const errObj = data as Record<string, unknown>
        const apiErr = (errObj.error || {}) as Record<string, unknown>
        const message =
          typeof apiErr.message === 'string' ? apiErr.message : 'Request failed'
        const type = typeof apiErr.type === 'string' ? apiErr.type : 'api'
        throw {
          status: response.status,
          type,
          message,
          userMessage: message,
        }
      }

      return data as T
    } catch (error: unknown) {
      clearTimeout(timeoutId)
      // If the request was aborted due to timeout, throw a timeout error
      if (
        error instanceof Error &&
        error.name === 'AbortError' &&
        controller.signal.aborted &&
        !options?.signal?.aborted
      ) {
        throw Object.assign(new Error('Request timeout. Please try again.'), {
          cause: error,
        })
      }
      // Re-throw other errors
      throw error
    }
  }

  // HTTP method wrappers
  public async get<T>(path: string, options?: Omit<RequestOptions, 'body'>) {
    return this.request<T>('GET', path, options)
  }

  public async post<T>(path: string, options?: RequestOptions) {
    return this.request<T>('POST', path, options)
  }

  public async put<T>(path: string, options?: RequestOptions) {
    return this.request<T>('PUT', path, options)
  }

  public async patch<T>(path: string, options?: RequestOptions) {
    return this.request<T>('PATCH', path, options)
  }

  public async delete<T>(path: string, options?: RequestOptions) {
    return this.request<T>('DELETE', path, options)
  }
}

// Utility function to get the correct API base URL
const getApiBaseUrl = (): string => {
  const injected =
    (typeof window !== 'undefined' && window.runtimeConfig?.apiBaseUrl) ||
    import.meta.env.VITE_API_BASE_URL ||
    ''
  return injected.trim()
}

// Create and export API instances
export const apiService = new ApiService(getApiBaseUrl(), '/api/v1')
