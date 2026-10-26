import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type Bucket = components['schemas']['Bucket']
export type BucketDetail = components['schemas']['BucketDetail']
export type BucketDeleted = components['schemas']['BucketDeleted']
type BucketList = components['schemas']['BucketList']
type CreateBucketRequest = components['schemas']['CreateBucketRequest']
type UpdateBucketRequest = components['schemas']['UpdateBucketRequest']
export type ObjectListing = components['schemas']['ObjectListing']
export type StorageObject = components['schemas']['StorageObject']
export type PresignedURL = components['schemas']['PresignedURL']

/** ListObjectsArgs names one page of one folder. */
export interface ListObjectsArgs {
  name: string
  prefix: string
  cursor?: string
}

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

    // Object listings are tagged per bucket, so an upload, a new folder or a
    // delete refreshes every page of that bucket the console holds.
    listObjects: build.query<ObjectListing, ListObjectsArgs>({
      query: ({ name, prefix, cursor }) => ({
        url: `/s3/buckets/${encodeURIComponent(name)}/objects`,
        params: { ...(prefix === '' ? {} : { prefix }), ...(cursor ? { cursor } : {}) },
      }),
      providesTags: (_result, _error, { name }) => [{ type: 'BucketObjects', id: name }],
    }),

    // The presigned URLs are mutations, not queries: each is a fresh grant
    // with its own expiry, and caching one would hand out a stale URL.
    presignUpload: build.mutation<
      PresignedURL,
      { name: string; key: string; content_type: string }
    >({
      query: ({ name, ...body }) => ({
        url: `/s3/buckets/${encodeURIComponent(name)}/uploads`,
        method: 'POST',
        body,
      }),
    }),

    presignDownload: build.mutation<PresignedURL, { name: string; key: string }>({
      query: ({ name, key }) => ({
        url: `/s3/buckets/${encodeURIComponent(name)}/downloads`,
        method: 'POST',
        body: { key },
      }),
    }),

    createFolder: build.mutation<{ prefix: string }, { name: string; prefix: string }>({
      query: ({ name, prefix }) => ({
        url: `/s3/buckets/${encodeURIComponent(name)}/folders`,
        method: 'POST',
        body: { prefix },
      }),
      invalidatesTags: (_result, _error, { name }) => [{ type: 'BucketObjects', id: name }],
    }),

    deleteObjects: build.mutation<void, { name: string; keys: string[] }>({
      query: ({ name, keys }) => ({
        url: `/s3/buckets/${encodeURIComponent(name)}/objects/delete`,
        method: 'POST',
        body: { keys },
      }),
      invalidatesTags: (_result, _error, { name }) => [{ type: 'BucketObjects', id: name }],
    }),
  }),
})

export const {
  useListBucketsQuery,
  useGetBucketQuery,
  useCreateBucketMutation,
  useUpdateBucketMutation,
  useDeleteBucketMutation,
  useListObjectsQuery,
  usePresignUploadMutation,
  usePresignDownloadMutation,
  useCreateFolderMutation,
  useDeleteObjectsMutation,
} = bucketsApi
