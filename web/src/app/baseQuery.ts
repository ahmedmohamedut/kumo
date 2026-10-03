import { fetchBaseQuery } from '@reduxjs/toolkit/query'

/**
 * The network layer of the console, without authentication.
 *
 * It lives apart from `app/api.ts` because both the RTK Query `baseQuery` and
 * the session code need it: the baseQuery retries through it after a refresh,
 * and `authSession` calls it directly, which keeps the refresh itself out of
 * the 401 handling it exists to satisfy.
 *
 * Every request stays on the console's own origin: in development Vite proxies
 * `/api` to kumod (see vite.config.ts), in production kumod serves the console
 * itself. Same origin is what makes the `SameSite=Strict` refresh cookie work.
 * The URL is spelled out rather than left relative because `fetch` only
 * accepts absolute URLs, and jsdom keeps Node's fetch.
 */
export const apiBaseUrl = `${window.location.origin}/api/v1`

export const rawBaseQuery = fetchBaseQuery({
  baseUrl: apiBaseUrl,
  // The refresh cookie is HttpOnly, so the browser only sends it back here.
  credentials: 'include',
  prepareHeaders: (headers) => {
    headers.set('accept', 'application/json')
    return headers
  },
})
