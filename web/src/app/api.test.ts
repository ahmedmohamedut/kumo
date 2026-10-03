import { beforeEach, describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'

import { kumoApi } from '@/app/api'
import { resetRefreshForTesting } from '@/app/authSession'
import { createStore } from '@/app/store'
import { signedIn } from '@/features/auth/authSlice'
import { server } from '@/mocks/server'
import { MOCK_ADMIN } from '@/mocks/handlers'

/**
 * Two endpoints that exist only to drive the baseQuery: one that 401s until the
 * session is refreshed, one that always does. They live in the test file so
 * the console keeps no probe endpoints.
 */
const probe = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    testProtected: build.query<{ ok: boolean }, void>({ query: () => '/test/protected' }),
    testForever: build.query<{ ok: boolean }, void>({ query: () => '/test/forever' }),
  }),
})

/** ok answers the probe once the request carries a token MSW knows. */
const okHandler = http.get('/api/v1/test/protected', ({ request }) => {
  const authorized = request.headers.get('authorization') === 'Bearer probe-token'
  return authorized
    ? HttpResponse.json({ ok: true })
    : HttpResponse.json(
        { error: { code: 'unauthenticated', message: 'the access token expired' } },
        { status: 401 },
      )
})

/** foreverHandler 401s even after a refresh, so the failure path can be tested. */
const foreverHandler = http.get('/api/v1/test/forever', () =>
  HttpResponse.json({ error: { code: 'unauthenticated', message: 'nope' } }, { status: 401 }),
)

/** countingRefresh counts rotations, which is what single-flight is about. */
function countingRefresh(): { calls: () => number } {
  const counter = { value: 0 }
  server.use(
    http.post('/api/v1/auth/refresh', () => {
      counter.value += 1
      // The real endpoint rotates the cookie; doing it here keeps the token
      // bookkeeping in the mock honest.
      return HttpResponse.json(
        { access_token: 'probe-token', expires_in: 900 },
        { headers: { 'Set-Cookie': 'kumo_refresh=probe; Path=/api/v1/auth' } },
      )
    }),
    http.get('/api/v1/auth/me', () => HttpResponse.json(MOCK_ADMIN)),
  )
  return { calls: () => counter.value }
}

beforeEach(() => {
  // The single-flight promise is module state, shared by every store in the
  // process; each test starts without one in flight.
  resetRefreshForTesting()
  server.use(okHandler, foreverHandler)
})

describe('the authenticated baseQuery', () => {
  it('attaches the access token to a request', async () => {
    const store = createStore()
    store.dispatch(signedIn({ user: MOCK_ADMIN, accessToken: 'probe-token' }))

    const result = await store.dispatch(probe.endpoints.testProtected.initiate())

    expect(result.data).toEqual({ ok: true })
    expect(result.error).toBeUndefined()
  })

  it('refreshes once on a 401 and replays the request', async () => {
    const refreshes = countingRefresh()
    const store = createStore()
    store.dispatch(signedIn({ user: MOCK_ADMIN, accessToken: 'stale-token' }))

    const result = await store.dispatch(probe.endpoints.testProtected.initiate())

    expect(result.data).toEqual({ ok: true })
    expect(refreshes.calls()).toBe(1)
    // The replay carried the fresh token, and the store kept it.
    expect(store.getState().auth.accessToken).toBe('probe-token')
  })

  it('shares one refresh between requests that 401 at the same time', async () => {
    const refreshes = countingRefresh()
    const store = createStore()
    store.dispatch(signedIn({ user: MOCK_ADMIN, accessToken: 'stale-token' }))

    const first = store.dispatch(probe.endpoints.testProtected.initiate())
    const second = store.dispatch(probe.endpoints.testProtected.initiate())
    const results = await Promise.all([first, second])

    // Two rotations of the same cookie would look like token reuse to the
    // backend and revoke the whole family.
    expect(refreshes.calls()).toBe(1)
    expect(results.map((result) => result.data)).toEqual([{ ok: true }, { ok: true }])
  })

  it('signs the operator out when the refresh fails', async () => {
    const store = createStore()
    store.dispatch(signedIn({ user: MOCK_ADMIN, accessToken: 'stale-token' }))
    server.use(
      http.post('/api/v1/auth/refresh', () =>
        HttpResponse.json(
          { error: { code: 'unauthenticated', message: 'the refresh token was revoked' } },
          { status: 401 },
        ),
      ),
    )

    const result = await store.dispatch(probe.endpoints.testForever.initiate())

    expect(result.error).toMatchObject({ status: 401 })
    expect(store.getState().auth.status).toBe('anonymous')
    expect(store.getState().auth.accessToken).toBeNull()
    expect(store.getState().auth.user).toBeNull()
  })

  it('does not retry twice: a second 401 is reported as is', async () => {
    const refreshes = countingRefresh()
    const store = createStore()
    store.dispatch(signedIn({ user: MOCK_ADMIN, accessToken: 'stale-token' }))

    const result = await store.dispatch(probe.endpoints.testForever.initiate())

    expect(result.error).toMatchObject({ status: 401 })
    expect(refreshes.calls()).toBe(1)
    // The session survives: the refresh worked, the endpoint is simply refused.
    expect(store.getState().auth.status).toBe('authenticated')
  })

  it('recovers a session from the cookie when a request 401s without one', async () => {
    const refreshes = countingRefresh()
    const store = createStore()

    const result = await store.dispatch(probe.endpoints.testForever.initiate())

    expect(result.error).toMatchObject({ status: 401 })
    // A request can beat the page-load refresh to the cookie; the baseQuery
    // spends it and publishes the session instead of bouncing the operator.
    expect(refreshes.calls()).toBe(1)
    expect(store.getState().auth.status).toBe('authenticated')
    expect(store.getState().auth.accessToken).toBe('probe-token')
  })
})
