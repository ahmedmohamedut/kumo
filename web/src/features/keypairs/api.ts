import { kumoApi, uncachedRequest, type UncachedStore } from '@/app/api'
import type { FetchBaseQueryError } from '@reduxjs/toolkit/query'
import type { components } from '@/types/api'

type KeyPair = components['schemas']['KeyPair']
type KeyPairList = components['schemas']['KeyPairList']
type CreateKeyPairRequest = components['schemas']['CreateKeyPairRequest']
type CreateKeyPairResponse = components['schemas']['CreateKeyPairResponse']
type ImportKeyPairRequest = components['schemas']['ImportKeyPairRequest']

/** The largest page the API will serve, so one request covers the whole list. */
export const MAX_PAGE_SIZE = 200

export interface ListKeyPairsArgs {
  /** Case-insensitive substring of the name. */
  query?: string
  /** `created_at:asc` or `created_at:desc`. */
  sort?: string
  pageToken?: string
}

/**
 * The key pair endpoints (api/openapi.yaml §/key-pairs).
 *
 * `createKeyPair` is the one to read twice. Its response is the only place a
 * generated private key ever exists on the client, and an RTK Query mutation
 * would park that result in the store for good — `keepUnusedDataFor` is a
 * query-only option, so there is no way to make a mutation forget. It therefore
 * goes through `uncachedRequest`, which reuses the authenticated baseQuery
 * (refresh on 401 included) and never caches the answer: the private key only
 * ever exists in the create page's component state, and only while the save
 * dialog is open (AGENTS.md security rules, 03-frontend.md §6.5).
 */
export const keyPairsApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listKeyPairs: build.query<KeyPairList, ListKeyPairsArgs | void>({
      query: (args) => ({
        url: '/key-pairs',
        params: {
          page_size: MAX_PAGE_SIZE,
          ...(args?.query ? { q: args.query } : {}),
          ...(args?.sort ? { sort: args.sort } : {}),
          ...(args?.pageToken ? { page_token: args.pageToken } : {}),
        },
        // Key pairs have no transitional state, so the slow poll of
        // 03-frontend.md §5 is the right one here.
        pollingInterval: 30000,
      }),
      providesTags: ['KeyPair'],
    }),

    importKeyPair: build.mutation<KeyPair, ImportKeyPairRequest>({
      query: (body) => ({ url: '/key-pairs/import', method: 'POST', body }),
      invalidatesTags: ['KeyPair'],
    }),

    deleteKeyPair: build.mutation<void, { id: string; force?: boolean }>({
      query: ({ id, force }) => ({
        url: `/key-pairs/${encodeURIComponent(id)}`,
        method: 'DELETE',
        // Only sent when asked for: `?force=false` would still read as a
        // deliberate shortcut to anything auditing the request.
        params: force === true ? { force: 'true' } : undefined,
      }),
      invalidatesTags: ['KeyPair'],
    }),
  }),
})

export const { useListKeyPairsQuery, useImportKeyPairMutation, useDeleteKeyPairMutation } =
  keyPairsApi

/** The result of createKeyPair: the one response that carries a private key. */
export type CreateKeyPairResult = { data: CreateKeyPairResponse } | { error: FetchBaseQueryError }

/**
 * createKeyPair generates a key pair without letting the response reach the query
 * cache. The caller owns the private key from here on: read it out of the result,
 * show it once, and drop it.
 *
 * The list is invalidated by hand because `uncachedRequest` produces no cache
 * entry for `invalidatesTags` to hang off.
 */
export async function createKeyPair(
  body: CreateKeyPairRequest,
  store: UncachedStore,
): Promise<CreateKeyPairResult> {
  const result = await uncachedRequest(
    { url: '/key-pairs', method: 'POST', body },
    store,
    'createKeyPair',
  )

  if ('error' in result) {
    return result
  }

  store.dispatch(kumoApi.util.invalidateTags(['KeyPair']))
  return { data: result.data as CreateKeyPairResponse }
}
