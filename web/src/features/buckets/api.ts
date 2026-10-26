import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type Bucket = components['schemas']['Bucket']
export type BucketDetail = components['schemas']['BucketDetail']
export type BucketDeleted = components['schemas']['BucketDeleted']
type BucketList = components['schemas']['BucketList']
type CreateBucketRequest = components['schemas']['CreateBucketRequest']
type UpdateBucketRequest = components['schemas']['UpdateBucketRequest']

/** DeleteBucketArgs is a delete; `force` empties the bucket first, as a task. */
export interface DeleteBucketArgs {
  name: string
  force: boolean
}

/**
 * pollInterval keeps a list or a detail page fresh while a forced delete is
 * emptying a bucket, and leaves it alone otherwise: a bucket has no other
 * transitional state, and the list is read from the object store on every
 * request, so idle polling would only add load to it.
 */
export function pollInterval(buckets: ReadonlyArray<Bucket> | undefined): number {
  return buckets?.some((bucket) => bucket.state === 'deleting') === true ? 5000 : 0
}

/** bucketResourceId is the id a bucket's tasks are filed under (02-data-model.md). */
export function bucketResourceId(name: string): string {
  return `bucket/${name}`
}

/**
 * The bucket endpoints (api/openapi.yaml §/s3/buckets). Every one answers
 * `503 unavailable` on a node without object storage; the pages read that
 * from `/system/info` first and say so instead of showing an error.
 */
export const bucketsApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listBuckets: build.query<Bucket[], void>({
      query: () => ({ url: '/s3/buckets' }),
      transformResponse: (response: BucketList) => response.items,
      providesTags: ['Bucket'],
    }),

    getBucket: build.query<BucketDetail, string>({
      query: (name) => ({ url: `/s3/buckets/${encodeURIComponent(name)}` }),
      providesTags: (_result, _error, name) => [{ type: 'Bucket', id: name }],
    }),

    createBucket: build.mutation<BucketDetail, CreateBucketRequest>({
      query: (body) => ({ url: '/s3/buckets', method: 'POST', body }),
      invalidatesTags: ['Bucket'],
    }),

    updateBucket: build.mutation<BucketDetail, { name: string } & UpdateBucketRequest>({
      query: ({ name, ...body }) => ({
        url: `/s3/buckets/${encodeURIComponent(name)}`,
        method: 'PATCH',
        body,
      }),
      invalidatesTags: (_result, _error, { name }) => [{ type: 'Bucket', id: name }, 'Bucket'],
    }),

    // A plain delete answers 204 with no body; a forced one 202 with the
    // bucket in `deleting` and its task.
    deleteBucket: build.mutation<BucketDeleted | null, DeleteBucketArgs>({
      query: ({ name, force }) => ({
        url: `/s3/buckets/${encodeURIComponent(name)}${force ? '?force=true' : ''}`,
        method: 'DELETE',
      }),
      invalidatesTags: (_result, _error, { name }) => [
        { type: 'Bucket', id: name },
        'Bucket',
        'Task',
      ],
    }),
  }),
})

export const {
  useListBucketsQuery,
  useGetBucketQuery,
  useCreateBucketMutation,
  useUpdateBucketMutation,
  useDeleteBucketMutation,
} = bucketsApi
