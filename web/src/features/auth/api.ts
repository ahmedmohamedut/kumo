import { kumoApi } from '@/app/api'
import { loggedOut, signedIn } from '@/features/auth/authSlice'
import type { components } from '@/types/api'

type AuthUser = components['schemas']['AuthUser']
type AuthLoginResponse = components['schemas']['AuthLoginResponse']

/**
 * The auth endpoints, plus the session bookkeeping around them.
 *
 * Each mutation starts the session itself instead of leaving the caller to
 * dispatch `signedIn`: the token must reach the store before the next request
 * goes out, and a page that forgets that would send an unauthenticated call and
 * bounce itself to the login page.
 */
export const authApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    login: build.mutation<AuthLoginResponse, components['schemas']['AuthLoginRequest']>({
      query: (body) => ({ url: '/auth/login', method: 'POST', body }),
      async onQueryStarted(_body, { dispatch, queryFulfilled }) {
        try {
          const { data } = await queryFulfilled
          dispatch(signedIn({ user: data.user, accessToken: data.access_token }))
        } catch {
          // The form reports the failure; RTK Query would otherwise see this
          // rejection as unhandled.
        }
      },
    }),

    // /auth/setup creates the first administrator but does not log it in, so the
    // console signs in right after with the same credentials.
    setup: build.mutation<AuthUser, components['schemas']['AuthSetupRequest']>({
      query: (body) => ({ url: '/auth/setup', method: 'POST', body }),
    }),

    logout: build.mutation<void, void>({
      query: () => ({ url: '/auth/logout', method: 'POST' }),
      async onQueryStarted(_body, { dispatch, queryFulfilled }) {
        try {
          await queryFulfilled
        } catch {
          // Signing out is local first: a refused request still ends the
          // session below.
        } finally {
          // The session is over locally whatever the server said: a failed
          // sign-out must not leave the operator looking at a console that can
          // no longer authenticate anything.
          dispatch(loggedOut())
          // And nothing cached for the previous user may survive it.
          dispatch(kumoApi.util.resetApiState())
        }
      },
    }),
  }),
})

export const { useLoginMutation, useSetupMutation, useLogoutMutation } = authApi

/** systemInfo answers who the control plane is; /setup is only reachable while it is open. */
export const systemApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    systemInfo: build.query<components['schemas']['SystemInfo'], void>({
      query: () => '/system/info',
    }),
  }),
})

export const { useSystemInfoQuery } = systemApi
