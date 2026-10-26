import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type Instance = components['schemas']['Instance']
export type InstanceList = components['schemas']['InstanceList']
export type InstanceState = components['schemas']['InstanceState']
export type LaunchInstanceRequest = components['schemas']['LaunchInstanceRequest']
export type UpdateInstanceRequest = components['schemas']['UpdateInstanceRequest']
export type ConsoleOutput = components['schemas']['ConsoleOutput']

/** The largest page the API will serve, so one request covers the whole list. */
export const MAX_PAGE_SIZE = 200

/**
 * transitional are the states where the node is still working on the instance.
 * They decide the poll interval, so a launch in flight keeps the table honest
 * without a busy 30 s loop forever afterwards (03-frontend.md §5).
 *
 * `running` is deliberately absent: a running instance is settled, and watching
 * one at 5 s would make a page of long-lived VMs the most expensive thing an
 * operator can leave open.
 */
const TRANSITIONAL: ReadonlySet<string> = new Set<InstanceState>([
  'pending',
  'stopping',
  'terminating',
])

/**
 * pollInterval is fast while an instance is moving and lazy once it has settled.
 *
 * The pages pass this to the hook rather than setting `pollingInterval` on the
 * endpoint, because RTK Query only types that option as a constant: the value
 * has to come from the cached copy of the query, read through
 * `endpoints.<name>.select(args)`, and a per-hook option is the supported way to
 * do that.
 */
export function pollInterval(items: ReadonlyArray<Instance> | undefined): number {
  return items?.some((instance) => TRANSITIONAL.has(instance.state)) === true ? 5000 : 30000
}

export interface ListInstancesArgs {
  /** Case-insensitive substring of the name (the API searches names only). */
  query?: string
  /** Restrict the page to one state. */
  state?: InstanceState
  /** `launched_at:asc` or `launched_at:desc`. */
  sort?: string
  pageToken?: string
}

/** LaunchInstanceArg is the launch body plus the `count` the API requires. */
export type LaunchInstanceArg = LaunchInstanceRequest

/** InstanceActionArg is `{id, force}` for stop and reboot. */
export interface InstanceActionArg {
  id: string
  /** Take the destructive shortcut instead of the graceful path. */
  force?: boolean
}

/**
 * The instance endpoints (api/openapi.yaml §Instances).
 *
 * Launch and every lifecycle action answer `202` with the instance in a
 * transitional state and a `Location` header naming the task that does the work,
 * so the console never blocks on a VM boot: the list and the detail page
 * invalidate on `Instance` and follow the state through the poll.
 *
 * The lifecycle mutations invalidate both the one instance and the whole list,
 * because an action moves a row out of the state an operator filters on and the
 * counters in the header are derived from the page.
 */
export const instancesApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listInstances: build.query<InstanceList, ListInstancesArgs | void>({
      query: (args) => ({
        url: '/instances',
        params: {
          page_size: MAX_PAGE_SIZE,
          ...(args?.query ? { q: args.query } : {}),
          ...(args?.state ? { state: args.state } : {}),
          ...(args?.sort ? { sort: args.sort } : {}),
          ...(args?.pageToken ? { page_token: args.pageToken } : {}),
        },
      }),
      providesTags: ['Instance'],
    }),

    getInstance: build.query<Instance, string>({
      query: (id) => ({ url: `/instances/${encodeURIComponent(id)}` }),
      providesTags: (_result, _error, id) => [{ type: 'Instance', id }],
    }),

    // The tail of the serial log. It is read on demand rather than polled: the
    // detail page asks for it once per Refresh press, because a guest that is not
    // booting writes nothing worth re-fetching.
    getInstanceConsoleOutput: build.query<ConsoleOutput, { id: string; tailKib?: number }>({
      query: ({ id, tailKib }) => ({
        url: `/instances/${encodeURIComponent(id)}/console-output`,
        params: { ...(tailKib === undefined ? {} : { tail_kib: tailKib }) },
      }),
      providesTags: (_result, _error, { id }) => [{ type: 'Instance', id }],
    }),

    launchInstance: build.mutation<Instance, LaunchInstanceArg>({
      query: (body) => ({ url: '/instances', method: 'POST', body }),
      invalidatesTags: ['Instance'],
    }),

    updateInstance: build.mutation<Instance, { id: string } & UpdateInstanceRequest>({
      query: ({ id, ...body }) => ({
        url: `/instances/${encodeURIComponent(id)}`,
        method: 'PATCH',
        body,
      }),
      invalidatesTags: (_result, _error, { id }) => [{ type: 'Instance', id }, 'Instance'],
    }),

    startInstance: build.mutation<Instance, string>({
      query: (id) => ({ url: `/instances/${encodeURIComponent(id)}/start`, method: 'POST' }),
      invalidatesTags: (_result, _error, id) => [{ type: 'Instance', id }, 'Instance'],
    }),

    stopInstance: build.mutation<Instance, InstanceActionArg>({
      query: ({ id, force }) => ({
        url: `/instances/${encodeURIComponent(id)}/stop`,
        method: 'POST',
        params: { ...(force === undefined ? {} : { force: String(force) }) },
      }),
      invalidatesTags: (_result, _error, { id }) => [{ type: 'Instance', id }, 'Instance'],
    }),

    rebootInstance: build.mutation<Instance, InstanceActionArg>({
      query: ({ id, force }) => ({
        url: `/instances/${encodeURIComponent(id)}/reboot`,
        method: 'POST',
        params: { ...(force === undefined ? {} : { force: String(force) }) },
      }),
      invalidatesTags: (_result, _error, { id }) => [{ type: 'Instance', id }, 'Instance'],
    }),

    // A terminate removes the row rather than marking it, so the list refetch
    // finds the instance gone and a repeat of the request answers `404` like any
    // other unknown id. The list invalidates either way.
    terminateInstance: build.mutation<Instance, string>({
      query: (id) => ({ url: `/instances/${encodeURIComponent(id)}`, method: 'DELETE' }),
      invalidatesTags: (_result, _error, id) => [{ type: 'Instance', id }, 'Instance'],
    }),
  }),
})

export const {
  useListInstancesQuery,
  useGetInstanceQuery,
  useGetInstanceConsoleOutputQuery,
  useLaunchInstanceMutation,
  useUpdateInstanceMutation,
  useStartInstanceMutation,
  useStopInstanceMutation,
  useRebootInstanceMutation,
  useTerminateInstanceMutation,
} = instancesApi
