import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type VPC = components['schemas']['VPC']
export type VPCDetail = components['schemas']['VPCDetail']
export type VPCList = components['schemas']['VPCList']
export type VPCWithTask = components['schemas']['VPCWithTask']
export type VPCForwardMode = components['schemas']['VPCForwardMode']
export type CreateVPCRequest = components['schemas']['CreateVPCRequest']
export type UpdateTagsRequest = components['schemas']['UpdateTagsRequest']

/** The largest page the API will serve, so one request covers the whole list. */
export const MAX_PAGE_SIZE = 200

export interface ListVPCsArgs {
  /** Case-insensitive substring of the name (the API searches names only). */
  query?: string
  /** `name:asc` or `created_at:desc`, the API's sort syntax. */
  sort?: string
  pageToken?: string
}

/**
 * The VPC endpoints (api/openapi.yaml §VPCs).
 *
 * A VPC is one libvirt network and one Linux bridge, so creating and deleting
 * one is a task: the create answers `202` with the row already in place and the
 * id of the task that brings the network up, and the delete answers `202` with
 * the task that destroys the network before the row goes away. The console
 * never blocks on `virsh net-create`.
 *
 * Deletes invalidate the whole list: the row disappears when the task runs, and
 * a 409 answers while any subnet or instance still lives inside the VPC.
 */
export const vpcsApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listVPCs: build.query<VPCList, ListVPCsArgs | void>({
      query: (args) => ({
        url: '/vpcs',
        params: {
          page_size: MAX_PAGE_SIZE,
          ...(args?.query ? { q: args.query } : {}),
          ...(args?.sort ? { sort: args.sort } : {}),
          ...(args?.pageToken ? { page_token: args.pageToken } : {}),
        },
      }),
      providesTags: ['VPC'],
    }),

    getVPC: build.query<VPCDetail, string>({
      query: (id) => ({ url: `/vpcs/${encodeURIComponent(id)}` }),
      providesTags: (_result, _error, id) => [{ type: 'VPC', id }],
    }),

    createVPC: build.mutation<VPCWithTask, CreateVPCRequest>({
      query: (body) => ({ url: '/vpcs', method: 'POST', body }),
      invalidatesTags: ['VPC'],
    }),

    // `name`, `cidr` and `mode` are immutable; only tags move (01-api.md §VPCs).
    updateVPC: build.mutation<VPC, { id: string } & UpdateTagsRequest>({
      query: ({ id, ...body }) => ({
        url: `/vpcs/${encodeURIComponent(id)}`,
        method: 'PATCH',
        body,
      }),
      invalidatesTags: (_result, _error, { id }) => [{ type: 'VPC', id }, 'VPC'],
    }),

    deleteVPC: build.mutation<VPCWithTask, string>({
      query: (id) => ({ url: `/vpcs/${encodeURIComponent(id)}`, method: 'DELETE' }),
      invalidatesTags: ['VPC'],
    }),
  }),
})

export const {
  useListVPCsQuery,
  useGetVPCQuery,
  useCreateVPCMutation,
  useUpdateVPCMutation,
  useDeleteVPCMutation,
} = vpcsApi
