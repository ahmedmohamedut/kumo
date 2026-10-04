import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type Image = components['schemas']['Image']
export type ImageList = components['schemas']['ImageList']
export type CatalogImage = components['schemas']['CatalogImage']
export type ImportImageRequest = components['schemas']['ImportImageRequest']
export type UpdateImageRequest = components['schemas']['UpdateImageRequest']
type ImageState = components['schemas']['ImageState']

/** The largest page the API will serve, so one request covers the whole list. */
export const MAX_PAGE_SIZE = 200

/**
 * transitional are the states where the node is still working on the image. They
 * decide the poll interval, so an import in flight keeps the table honest
 * without a busy 30 s loop forever afterwards (03-frontend.md §5).
 */
const TRANSITIONAL: ReadonlySet<string> = new Set<ImageState>([
  'pending',
  'downloading',
  'deleting',
])

/**
 * pollInterval is fast while an image is moving and lazy once it has settled.
 *
 * The pages pass this to the hook rather than setting `pollingInterval` on the
 * endpoint, because RTK Query only types that option as a constant: the value
 * has to come from the data the page already has, and a per-hook option is
 * exactly the supported way to do that.
 */
export function pollInterval(items: ReadonlyArray<Image> | undefined): number {
  return items?.some((image) => TRANSITIONAL.has(image.state)) === true ? 5000 : 30000
}

export interface ListImagesArgs {
  /** Case-insensitive substring of the name (the API searches names only). */
  query?: string
  /** `created_at:asc` or `created_at:desc`. */
  sort?: string
  /** Restrict the page to one state. */
  state?: ImageState
  pageToken?: string
}

/**
 * The image endpoints (api/openapi.yaml §/images).
 *
 * Import and delete both answer `202` with the image and the id of the task doing
 * the work, so the console can follow one task instead of polling every row: the
 * list and the detail page invalidate on `Image` and `Task` tags, and the task
 * view polls while it is unfinished.
 */
export const imagesApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listImages: build.query<ImageList, ListImagesArgs | void>({
      query: (args) => ({
        url: '/images',
        params: {
          page_size: MAX_PAGE_SIZE,
          ...(args?.query ? { q: args.query } : {}),
          ...(args?.sort ? { sort: args.sort } : {}),
          ...(args?.state ? { state: args.state } : {}),
          ...(args?.pageToken ? { page_token: args.pageToken } : {}),
        },
      }),
      providesTags: ['Image'],
    }),

    // A single image still moves while it is importing, so the detail page
    // subscribes with `pollInterval` too.
    getImage: build.query<Image, string>({
      query: (id) => ({ url: `/images/${encodeURIComponent(id)}` }),
      providesTags: (_result, _error, id) => [{ type: 'Image', id }],
    }),

    // The catalog is static configuration, so it is fetched once and never
    // refetched behind the operator's back.
    listCatalogImages: build.query<CatalogImage[], void>({
      query: () => ({ url: '/images/catalog' }),
      transformResponse: (response: { items: CatalogImage[] }) => response.items,
    }),

    importImage: build.mutation<Image, ImportImageRequest>({
      query: (body) => ({ url: '/images/import', method: 'POST', body }),
      invalidatesTags: ['Image'],
    }),

    updateImage: build.mutation<Image, { id: string } & UpdateImageRequest>({
      query: ({ id, ...body }) => ({
        url: `/images/${encodeURIComponent(id)}`,
        method: 'PATCH',
        body,
      }),
      invalidatesTags: (_result, _error, { id }) => [{ type: 'Image', id }, 'Image'],
    }),

    deleteImage: build.mutation<Image, string>({
      query: (id) => ({ url: `/images/${encodeURIComponent(id)}`, method: 'DELETE' }),
      invalidatesTags: ['Image'],
    }),

    retryImageTask: build.mutation<Image, string>({
      query: (id) => ({ url: `/images/${encodeURIComponent(id)}/retry`, method: 'POST' }),
      invalidatesTags: ['Image'],
    }),
  }),
})

export const {
  useListImagesQuery,
  useGetImageQuery,
  useListCatalogImagesQuery,
  useImportImageMutation,
  useUpdateImageMutation,
  useDeleteImageMutation,
  useRetryImageTaskMutation,
} = imagesApi
