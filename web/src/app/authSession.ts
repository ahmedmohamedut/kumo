import type { Dispatch as ReduxDispatch } from '@reduxjs/toolkit'

import { apiBaseUrl } from '@/app/baseQuery'
import { loggedOut, signedIn, type AuthUser } from '@/features/auth/authSlice'
import type { components } from '@/types/api'

type RefreshResponse = components['schemas']['AuthRefreshResponse']

/**
 * One refresh at a time.
 *
 * A page load can fire several requests at once (the nav polls, a mutation, a
 * refetch), and each of them hitting a 401 would rotate the refresh token
 * independently. The backend treats a replayed refresh token as a stolen one
 * and revokes the whole family, so a burst of 401s would log the operator out
 * for being busy. Callers that arrive while a refresh is in flight await the
 * same promise instead.
 */
let inFlight: Promise<AuthUser | null> | null = null

/**
 * refreshSession restores the session from the refresh cookie, the way a page
 * load does: rotate the token, read the user, publish both. It resolves to null
 * when the cookie is missing, expired or was revoked, which is the normal case
 * for an anonymous browser.
 */
export function refreshSession(dispatch: Dispatch): Promise<AuthUser | null> {
  if (!inFlight) {
    const flight = performRefresh(dispatch)
    inFlight = flight
    void flight.finally(() => {
      if (inFlight === flight) {
        inFlight = null
      }
    })
  }
  return inFlight
}

/** Dispatch is what refreshSession needs from the store, kept narrow on purpose. */
export type Dispatch = ReduxDispatch

/** resetRefreshForTesting drops the shared promise; only tests should need it. */
export function resetRefreshForTesting(): void {
  inFlight = null
}

async function performRefresh(dispatch: Dispatch): Promise<AuthUser | null> {
  // Plain `fetch`, not RTK Query: the refresh is the thing every endpoint's 401
  // handling depends on, so it cannot itself go through a baseQuery, and its
  // answer must not land in the query cache.
  const rotated = await fetch(`${apiBaseUrl}/auth/refresh`, {
    method: 'POST',
    credentials: 'include',
    headers: { accept: 'application/json' },
  })
  if (!rotated.ok) {
    dispatch(loggedOut())
    return null
  }

  const { access_token: accessToken } = (await rotated.json()) as RefreshResponse

  // /auth/refresh returns only the token, so the user comes from /auth/me: the
  // cookie alone can prove the session, not say who it belongs to.
  const me = await fetch(`${apiBaseUrl}/auth/me`, {
    credentials: 'include',
    headers: { accept: 'application/json', authorization: `Bearer ${accessToken}` },
  })
  if (!me.ok) {
    dispatch(loggedOut())
    return null
  }

  const user = (await me.json()) as AuthUser
  dispatch(signedIn({ user, accessToken }))
  return user
}
