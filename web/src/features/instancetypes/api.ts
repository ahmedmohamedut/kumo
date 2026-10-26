import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type InstanceType = components['schemas']['InstanceType']
export type InstanceTypeDetail = components['schemas']['InstanceTypeDetail']
type InstanceTypeList = components['schemas']['InstanceTypeList']
type CreateInstanceTypeRequest = components['schemas']['CreateInstanceTypeRequest']
type UpdateInstanceTypeRequest = components['schemas']['UpdateInstanceTypeRequest']

/**
 * The instance type endpoints (api/openapi.yaml §/instance-types).
 *
 * The list feeds both this feature's table and the launch form's picker, so it
 * carries the `InstanceType` tag: an administrator who edits a size here sees
 * the launch form offer the new one without a reload. Types have no
 * transitional state and change only when someone edits them, so nothing polls.
 */
export const instanceTypesApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    // `transformResponse` unwraps the list so callers read `types.find(...)`
    // rather than `types.items.find(...)`.
    listInstanceTypes: build.query<InstanceType[], void>({
      query: () => ({ url: '/instance-types' }),
      transformResponse: (response: InstanceTypeList) => response.items,
      providesTags: ['InstanceType'],
    }),

    // The detail carries `used_by`, which is what the delete dialog shows: the
    // API refuses a delete while any instance references the type.
    getInstanceType: build.query<InstanceTypeDetail, string>({
      query: (id) => ({ url: `/instance-types/${encodeURIComponent(id)}` }),
      providesTags: (_result, _error, id) => [{ type: 'InstanceType', id }],
    }),

    createInstanceType: build.mutation<InstanceType, CreateInstanceTypeRequest>({
      query: (body) => ({ url: '/instance-types', method: 'POST', body }),
      invalidatesTags: ['InstanceType'],
    }),

    updateInstanceType: build.mutation<InstanceType, { id: string } & UpdateInstanceTypeRequest>({
      query: ({ id, ...body }) => ({
        url: `/instance-types/${encodeURIComponent(id)}`,
        method: 'PATCH',
        body,
      }),
      invalidatesTags: (_result, _error, { id }) => [{ type: 'InstanceType', id }, 'InstanceType'],
    }),

    deleteInstanceType: build.mutation<void, string>({
      query: (id) => ({ url: `/instance-types/${encodeURIComponent(id)}`, method: 'DELETE' }),
      invalidatesTags: (_result, _error, id) => [{ type: 'InstanceType', id }, 'InstanceType'],
    }),
  }),
})

export const {
  useListInstanceTypesQuery,
  useGetInstanceTypeQuery,
  useCreateInstanceTypeMutation,
  useUpdateInstanceTypeMutation,
  useDeleteInstanceTypeMutation,
} = instanceTypesApi
