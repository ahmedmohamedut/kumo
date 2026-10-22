import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type SystemCapacity = components['schemas']['SystemCapacity']
export type CapacityMetric = components['schemas']['CapacityMetric']

/**
 * The node's own endpoints (api/openapi.yaml §/system).
 *
 * Capacity is not tagged. It is derived from the instances — `total` measured by
 * the hypervisor, `allocated` summed from the rows (00-architecture.md §6) — so
 * there is no entity for a mutation to invalidate, and a launch changes it
 * without touching anything called "capacity". The pages that show it poll
 * instead, which is the same way they learn an instance has booted.
 */
export const systemCapacityApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    systemCapacity: build.query<SystemCapacity, void>({
      query: () => '/system/capacity',
    }),
  }),
})

export const { useSystemCapacityQuery } = systemCapacityApi
