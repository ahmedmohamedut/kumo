import { createApi, fetchBaseQuery } from '@reduxjs/toolkit/query/react'

/**
 * The single RTK Query API for the console. Feature slices add their endpoints
 * with `kumoApi.injectEndpoints`, so `refetchOnFocus` and the cache invalidation
 * graph are shared.
 *
 * The base URL is relative: in development Vite proxies `/api` to kumod
 * (see vite.config.ts), in production the console is served by kumod itself.
 */
export const kumoApi = createApi({
  reducerPath: 'kumoApi',
  baseQuery: fetchBaseQuery({
    baseUrl: '/api/v1',
    credentials: 'include',
    prepareHeaders: (headers) => {
      headers.set('accept', 'application/json')
      return headers
    },
  }),
  tagTypes: ['Instance', 'Image', 'KeyPair', 'Task'],
  endpoints: () => ({}),
})
