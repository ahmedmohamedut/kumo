import { describe, expect, it } from 'vitest'

import type { PayloadAction } from '@reduxjs/toolkit'
import {
  authReducer,
  loggedOut,
  selectAccessToken,
  selectIsAdmin,
  selectSessionStatus,
  selectUser,
  signedIn,
  tokenRefreshed,
} from '@/features/auth/authSlice'
import type { AuthState } from '@/features/auth/authSlice'
import { MOCK_ADMIN, MOCK_VIEWER } from '@/mocks/handlers'

/** root wraps a slice state the way the store holds it, so selectors can read it. */
function root(state: AuthState): { auth: AuthState } {
  return { auth: state }
}

/** after applies an action to a session built from earlier actions. */
function after(...actions: PayloadAction<unknown>[]): AuthState {
  return actions.reduce<AuthState>(
    (state, action) => authReducer(state, action),
    authReducer(undefined, { type: '@@init' }),
  )
}

describe('authSlice', () => {
  it('starts unknown, because the refresh cookie decides', () => {
    const state = root(after())

    expect(selectSessionStatus(state)).toBe('unknown')
    expect(selectUser(state)).toBeNull()
    expect(selectAccessToken(state)).toBeNull()
  })

  it('signedIn publishes the user and the token', () => {
    const state = root(after(signedIn({ user: MOCK_ADMIN, accessToken: 'access-1' })))

    expect(selectSessionStatus(state)).toBe('authenticated')
    expect(selectUser(state)).toEqual(MOCK_ADMIN)
    expect(selectAccessToken(state)).toBe('access-1')
    expect(selectIsAdmin(state)).toBe(true)
  })

  it('signedIn does not mutate the previous state', () => {
    const before = after(signedIn({ user: MOCK_ADMIN, accessToken: 'a1' }))
    const next = authReducer(before, signedIn({ user: MOCK_VIEWER, accessToken: 'a2' }))

    expect(before.accessToken).toBe('a1')
    expect(next.accessToken).toBe('a2')
  })

  it('tokenRefreshed keeps the session while the user is known', () => {
    const state = root(
      after(signedIn({ user: MOCK_ADMIN, accessToken: 'access-1' }), tokenRefreshed(MOCK_ADMIN)),
    )

    expect(selectSessionStatus(state)).toBe('authenticated')
    expect(selectAccessToken(state)).toBe('access-1')
  })

  it('tokenRefreshed without a user drops back to anonymous', () => {
    const state = root(
      after(signedIn({ user: MOCK_ADMIN, accessToken: 'access-1' }), tokenRefreshed(null)),
    )

    expect(selectSessionStatus(state)).toBe('anonymous')
    expect(selectAccessToken(state)).toBeNull()
  })

  it('loggedOut forgets the user, the token and the viewer flag', () => {
    const state = root(after(signedIn({ user: MOCK_VIEWER, accessToken: 'access-1' }), loggedOut()))

    expect(selectSessionStatus(state)).toBe('anonymous')
    expect(selectUser(state)).toBeNull()
    expect(selectAccessToken(state)).toBeNull()
    expect(selectIsAdmin(state)).toBe(false)
  })
})
