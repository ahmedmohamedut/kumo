import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type Subnet = components['schemas']['Subnet']
export type SubnetList = components['schemas']['SubnetList']
export type CreateSubnetRequest = components['schemas']['CreateSubnetRequest']
export type UpdateTagsRequest = components['schemas']['UpdateTagsRequest']

/** The largest page the API will serve, so one request covers the whole list. */
export const MAX_PAGE_SIZE = 200

export interface ListSubnetsArgs {
  /** Restrict the page to one VPC. */
  vpcId?: string
  /** Case-insensitive substring of the name (the API searches names only). */
  query?: string
  /** `name:asc` or `created_at:desc`, the API's sort syntax. */
  sort?: string
  pageToken?: string
}

/**
 * The subnet endpoints (api/openapi.yaml §Subnets).
 *
 * Unlike a VPC, a subnet is just a row: creating one answers `201` with the
 * finished resource and deleting one answers `204`. There is no libvirt object
 * to bring up, so no task ever gets involved. Deleting refuses with a `409`
 * while any address is still allocated from the subnet.
 */
export const subnetsApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listSubnets: build.query<SubnetList, ListSubnetsArgs | void>({
      query: (args) => ({
        url: '/subnets',
        params: {
          page_size: MAX_PAGE_SIZE,
          ...(args?.vpcId ? { vpc_id: args.vpcId } : {}),
          ...(args?.query ? { q: args.query } : {}),
          ...(args?.sort ? { sort: args.sort } : {}),
          ...(args?.pageToken ? { page_token: args.pageToken } : {}),
        },
      }),
      providesTags: ['Subnet'],
    }),

    getSubnet: build.query<Subnet, string>({
      query: (id) => ({ url: `/subnets/${encodeURIComponent(id)}` }),
      providesTags: (_result, _error, id) => [{ type: 'Subnet', id }],
    }),

    createSubnet: build.mutation<Subnet, CreateSubnetRequest>({
      query: (body) => ({ url: '/subnets', method: 'POST', body }),
      invalidatesTags: ['Subnet'],
    }),

    // `name`, `cidr` and `vpc_id` are immutable; only tags move (01-api.md §Subnets).
    updateSubnet: build.mutation<Subnet, { id: string } & UpdateTagsRequest>({
      query: ({ id, ...body }) => ({
        url: `/subnets/${encodeURIComponent(id)}`,
        method: 'PATCH',
        body,
      }),
      invalidatesTags: (_result, _error, { id }) => [{ type: 'Subnet', id }, 'Subnet'],
    }),

    deleteSubnet: build.mutation<{ id: string }, string>({
      query: (id) => ({ url: `/subnets/${encodeURIComponent(id)}`, method: 'DELETE' }),
      invalidatesTags: ['Subnet'],
    }),
  }),
})

export const {
  useListSubnetsQuery,
  useGetSubnetQuery,
  useCreateSubnetMutation,
  useUpdateSubnetMutation,
  useDeleteSubnetMutation,
} = subnetsApi
