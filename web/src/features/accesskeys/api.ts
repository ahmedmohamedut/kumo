import type { FetchBaseQueryError } from '@reduxjs/toolkit/query'

import { kumoApi, uncachedRequest, type UncachedStore } from '@/app/api'
import type { components } from '@/types/api'

export type AccessKey = components['schemas']['AccessKey']
export type CreatedAccessKey = components['schemas']['CreatedAccessKey']
export type AccessLevel = components['schemas']['AccessLevel']
type AccessKeyList = components['schemas']['AccessKeyList']
type CreateAccessKeyRequest = components['schemas']['CreateAccessKeyRequest']
type UpdateAccessKeyRequest = components['schemas']['UpdateAccessKeyRequest']

/** LEVEL_LABEL is how the console names an access level. */
export const LEVEL_LABEL: Record<AccessLevel, string> = {
  'read-only': 'Read only',
  'read-write': 'Read and write',
}

/**
 * The access key endpoints (api/openapi.yaml §/s3/access-keys).
 *
 * Creating a key is not here: its answer carries the secret, and a mutation
 * result stays in the store for its whole life. It goes through
 * `createAccessKey` below instead, the way a generated private key does
 * (03-frontend.md §6.5).
 */
export const accessKeysApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listAccessKeys: build.query<AccessKey[], void>({
      query: () => ({ url: '/s3/access-keys' }),
      transformResponse: (response: AccessKeyList) => response.items,
      providesTags: ['AccessKey'],
    }),

    getAccessKey: build.query<AccessKey, string>({
      query: (id) => ({ url: `/s3/access-keys/${encodeURIComponent(id)}` }),
      providesTags: (_result, _error, id) => [{ type: 'AccessKey', id }],
    }),

    updateAccessKey: build.mutation<AccessKey, { id: string } & UpdateAccessKeyRequest>({
      query: ({ id, ...body }) => ({
        url: `/s3/access-keys/${encodeURIComponent(id)}`,
        method: 'PATCH',
        body,
      }),
      invalidatesTags: (_result, _error, { id }) => [{ type: 'AccessKey', id }, 'AccessKey'],
    }),

    deleteAccessKey: build.mutation<void, string>({
      query: (id) => ({ url: `/s3/access-keys/${encodeURIComponent(id)}`, method: 'DELETE' }),
      invalidatesTags: (_result, _error, id) => [{ type: 'AccessKey', id }, 'AccessKey'],
    }),
  }),
})

export const {
  useListAccessKeysQuery,
  useGetAccessKeyQuery,
  useUpdateAccessKeyMutation,
  useDeleteAccessKeyMutation,
} = accessKeysApi

/** The result of createAccessKey: the one response that carries a secret. */
export type CreateAccessKeyResult = { data: CreatedAccessKey } | { error: FetchBaseQueryError }

/**
 * createAccessKey issues a key without letting the answer reach the query
 * cache. The caller holds the secret in component state while the dialog
 * shows it, and nowhere else.
 */
export async function createAccessKey(
  body: CreateAccessKeyRequest,
  store: UncachedStore,
): Promise<CreateAccessKeyResult> {
  const result = await uncachedRequest(
    { url: '/s3/access-keys', method: 'POST', body },
    store,
    'createAccessKey',
  )
  if ('error' in result) {
    return result
  }
  store.dispatch(kumoApi.util.invalidateTags(['AccessKey']))
  return { data: result.data as CreatedAccessKey }
}
