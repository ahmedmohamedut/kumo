import { createSlice, type PayloadAction } from '@reduxjs/toolkit'

import type { components } from '@/types/api'

export type AuthUser = components['schemas']['AuthUser']
export type UserRole = components['schemas']['UserRole']

/**
 * Where the console is in the session lifecycle. `unknown` covers the window
 * between a page load and the refresh attempt that decides whether the cookie in
 * the browser still buys a session: nothing may redirect until it is over, or a
 * reload would bounce an authenticated operator to the login page.
 */
export type SessionStatus = 'unknown' | 'authenticated' | 'anonymous'

export interface AuthState {
  user: AuthUser | null
  /** Memory only: never persisted, so an XSS cannot read it from storage. */
  accessToken: string | null
  status: SessionStatus
}

const initialState: AuthState = {
  user: null,
  accessToken: null,
  status: 'unknown',
}

const authSlice = createSlice({
  name: 'auth',
  initialState,
  reducers: {
    /** signedIn starts a session from /auth/login or /auth/setup. */
    signedIn(state, action: PayloadAction<{ user: AuthUser; accessToken: string }>) {
      state.user = action.payload.user
      state.accessToken = action.payload.accessToken
      state.status = 'authenticated'
    },
    /**
     * tokenRefreshed replaces the access token after a rotation. The refresh
     * endpoint does not return the user, so the caller passes along what it
     * read from /auth/me.
     */
    tokenRefreshed(state, action: PayloadAction<AuthUser | null>) {
      state.accessToken = action.payload === null ? null : state.accessToken
      if (action.payload === null) {
        state.user = null
        state.status = 'anonymous'
        return
      }
      state.user = action.payload
      state.status = 'authenticated'
    },
    /** loggedOut drops the session after a failed refresh or a sign-out. */
    loggedOut(state) {
      state.user = null
      state.accessToken = null
      state.status = 'anonymous'
    },
  },
})

export const { signedIn, tokenRefreshed, loggedOut } = authSlice.actions

/** authReducer is the slice reducer, wired into the store. */
export const authReducer = authSlice.reducer

/** selectAuth is the whole session state, for components that need it all. */
export function selectAuth(state: { auth: AuthState }): AuthState {
  return state.auth
}

/** selectUser is the signed-in user, or null while anonymous. */
export function selectUser(state: { auth: AuthState }): AuthUser | null {
  return state.auth.user
}

/** selectAccessToken is the bearer token the baseQuery attaches. */
export function selectAccessToken(state: { auth: AuthState }): string | null {
  return state.auth.accessToken
}

/** selectSessionStatus gates the protected routes. */
export function selectSessionStatus(state: { auth: AuthState }): SessionStatus {
  return state.auth.status
}

/** selectIsAdmin is what write actions check before they render enabled. */
export function selectIsAdmin(state: { auth: AuthState }): boolean {
  return state.auth.user?.role === 'admin'
}
