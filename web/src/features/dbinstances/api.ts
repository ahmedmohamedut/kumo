import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type DBInstance = components['schemas']['DBInstance']
export type DBInstanceState = DBInstance['state']
export type CreateDBInstanceRequest = components['schemas']['CreateDBInstanceRequest']
type DBInstanceList = components['schemas']['DBInstanceList']

/** ListDBInstancesArgs selects one page of the list. */
export interface ListDBInstancesArgs {
  pageSize?: number
}

/**
 * The transitional states: a DB instance in one of them is being worked on by
 * a task or is waiting for its engine, and the page polls until it settles.
 */
const TRANSITIONAL: ReadonlySet<DBInstanceState> = new Set<DBInstanceState>([
  'creating',
  'starting',
  'stopping',
  'rebooting',
  'deleting',
])

/**
 * pollInterval keeps a list or a detail page fresh while anything is moving.
 *
 * A create takes as long as a first boot that builds a cluster, and a stop or
 * a start takes as long as the guest does, so the page follows them at 5 s and
 * drops to 30 s once everything has settled (03-frontend.md §5). `failed` is
 * not transitional, but it is polled all the same: the reconciler publishes a
 * database whose engine answered late, and that is the one state an operator
 * is waiting to see change on its own.
 */
export function pollInterval(items: ReadonlyArray<DBInstance> | undefined): number {
  if (items === undefined) {
    return 30000
  }
  return items.some((item) => TRANSITIONAL.has(item.state) || item.state === 'failed')
    ? 5000
    : 30000
}

/**
 * The DB instance endpoints (api/openapi.yaml §/rds/db-instances). Every one
 * answers `503 unavailable` on a node with the databases module off; the pages
 * read that from `/system/info` first and say so instead of showing an error.
 *
 * Each lifecycle mutation invalidates the DB instance and `Task`: the work
 * happens on the server afterwards, and the task is what the console follows.
 * `Instance` is invalidated too, because the hidden server behind a DB
 * instance still holds the node's vCPUs, memory and disk that
 * `/system/capacity` reports.
 */
export const dbInstancesApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listDBInstances: build.query<DBInstanceList, ListDBInstancesArgs | void>({
      query: (args) => ({
        url: '/rds/db-instances',
        params: { page_size: args?.pageSize ?? 100 },
      }),
      providesTags: ['DBInstance'],
    }),

    getDBInstance: build.query<DBInstance, string>({
      query: (id) => ({ url: `/rds/db-instances/${id}` }),
      providesTags: (_result, _error, id) => [{ type: 'DBInstance', id }],
    }),

    createDBInstance: build.mutation<DBInstance, CreateDBInstanceRequest>({
      query: (body) => ({ url: '/rds/db-instances', method: 'POST', body }),
      invalidatesTags: ['DBInstance', 'Task', 'Instance'],
    }),

    stopDBInstance: build.mutation<DBInstance, string>({
      query: (id) => ({ url: `/rds/db-instances/${id}/stop`, method: 'POST' }),
      invalidatesTags: (_result, _error, id) => [{ type: 'DBInstance', id }, 'DBInstance', 'Task'],
    }),

    startDBInstance: build.mutation<DBInstance, string>({
      query: (id) => ({ url: `/rds/db-instances/${id}/start`, method: 'POST' }),
      invalidatesTags: (_result, _error, id) => [{ type: 'DBInstance', id }, 'DBInstance', 'Task'],
    }),

    rebootDBInstance: build.mutation<DBInstance, string>({
      query: (id) => ({ url: `/rds/db-instances/${id}/reboot`, method: 'POST' }),
      invalidatesTags: (_result, _error, id) => [{ type: 'DBInstance', id }, 'DBInstance', 'Task'],
    }),

    deleteDBInstance: build.mutation<DBInstance, string>({
      query: (id) => ({ url: `/rds/db-instances/${id}`, method: 'DELETE' }),
      invalidatesTags: (_result, _error, id) => [
        { type: 'DBInstance', id },
        'DBInstance',
        'Task',
        'Instance',
      ],
    }),
  }),
})

export const {
  useListDBInstancesQuery,
  useGetDBInstanceQuery,
  useCreateDBInstanceMutation,
  useStopDBInstanceMutation,
  useStartDBInstanceMutation,
  useRebootDBInstanceMutation,
  useDeleteDBInstanceMutation,
} = dbInstancesApi
