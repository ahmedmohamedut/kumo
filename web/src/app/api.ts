import { createApi } from '@reduxjs/toolkit/query/react'
import type { BaseQueryFn, FetchArgs, FetchBaseQueryError } from '@reduxjs/toolkit/query'

import { rawBaseQuery } from '@/app/baseQuery'
import { refreshSession } from '@/app/authSession'
import { selectAccessToken } from '@/features/auth/authSlice'
import type { AppDispatch, RootState } from '@/app/store'

/**
 * The endpoints that must never trigger a refresh: their own 401 is the answer.
 * A wrong password would otherwise cost a refresh round trip and dispatch a
 * sign-out the login page has to ignore.
 */
const SKIP_REFRESH = new Set(['login', 'setup', 'refresh', 'logout'])

/** readEndpoint is the endpoint name RTK Query passes to the baseQuery. */
function readEndpoint(extra: unknown): string | undefined {
  if (typeof extra === 'object' && extra !== null && 'endpoint' in extra) {
    const { endpoint } = extra as { endpoint?: unknown }
    return typeof endpoint === 'string' ? endpoint : undefined
  }
  return undefined
}

/**
 * withBearer adds the access token, when there is one, to a request. RTK Query
 * types `headers` more loosely than `fetch` accepts, and every endpoint in this
 * console passes a plain object, so the cast is safe.
 */
function withBearer(request: FetchArgs, accessToken: string): FetchArgs {
  const headers = new Headers(request.headers as HeadersInit | undefined)
  headers.set('authorization', `Bearer ${accessToken}`)
  return { ...request, headers }
}

/**
 * The authenticated baseQuery of the console.
 *
 * It attaches the in-memory access token, and on a 401 it rotates the refresh
 * token once and replays the request — the access token is valid for 15 minutes
 * while a console page may stay open for days, so the expiry is the normal case,
 * not the exceptional one. The refresh is single-flight (`authSession`), and a
 * failure there dispatches `loggedOut`, which is what `RequireAuth` turns into
 * the redirect to `/login?next=…`.
 */
const withAuth: BaseQueryFn<string | FetchArgs, unknown, FetchBaseQueryError> = async (
  args,
  api,
  extra,
) => {
  const request: FetchArgs = typeof args === 'string' ? { url: args } : args
  const token = selectAccessToken(api.getState() as RootState)
  const result = await rawBaseQuery(
    token === null ? request : withBearer(request, token),
    api,
    extra,
  )

  if (result.error?.status !== 401) {
    return result
  }

  if (SKIP_REFRESH.has(readEndpoint(extra) ?? '')) {
    return result
  }

  const user = await refreshSession(api.dispatch as AppDispatch)
  if (user === null) {
    return result
  }

  const retried = selectAccessToken(api.getState() as RootState)
  if (retried === null) {
    return result
  }
  return rawBaseQuery(withBearer(request, retried), api, extra)
}

/**
 * The single RTK Query API for the console. Feature slices add their endpoints
 * with `kumoApi.injectEndpoints`, so `refetchOnFocus` and the cache invalidation
 * graph are shared.
 */
export const kumoApi = createApi({
  reducerPath: 'kumoApi',
  baseQuery: withAuth,
  tagTypes: ['Instance', 'Image', 'KeyPair', 'Task', 'VPC', 'Subnet'],
  endpoints: () => ({}),
})

/** Store is the slice of the Redux store `uncachedRequest` needs. */
export interface UncachedStore {
  getState: () => RootState
  dispatch: AppDispatch
}

/**
 * uncachedRequest runs one request through the same authenticated baseQuery as
 * every other call, but hands the response straight back instead of letting RTK
 * Query keep it.
 *
 * A mutation's result lives in the store for as long as the store does:
 * `keepUnusedDataFor` is a query-only option, and the trigger's `reset()` clears
 * the hook's view of the result rather than the cached entry. That is the right
 * trade for an instance id and the wrong one for a private key — the create
 * response is the only place a generated key exists, so it must not outlive the
 * dialog showing it (03-frontend.md §6.5).
 *
 * The 401 handling is `withAuth` itself, so an expired access token still
 * triggers a refresh and a replay exactly as it would for a cached endpoint.
 */
export async function uncachedRequest(
  request: FetchArgs,
  store: UncachedStore,
  endpointName: string,
): Promise<{ data: unknown; meta?: unknown } | { error: FetchBaseQueryError }> {
  // `fetchBaseQuery` reads getState/dispatch/signal off its second argument and
  // nothing else at runtime; the rest exist only to satisfy its type. The abort
  // controller gives the request a real signal to be cancelled through.
  const api = {
    getState: store.getState,
    dispatch: store.dispatch,
    extra: undefined,
    endpoint: endpointName,
    forced: undefined,
    type: 'query',
    signal: new AbortController().signal,
  }

  return withAuth(request, api as unknown as Parameters<typeof withAuth>[1], {
    endpoint: endpointName,
  })
}
