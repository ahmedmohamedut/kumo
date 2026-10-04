import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type Task = components['schemas']['Task']
export type TaskList = components['schemas']['TaskList']
type TaskState = components['schemas']['TaskState']

/** The largest page the API will serve, so one request covers the whole list. */
export const MAX_PAGE_SIZE = 200

/**
 * pollInterval is fast while a task is running and lazy once the queue is quiet.
 * As on the images endpoints, the page passes it to the hook: RTK Query types
 * `pollingInterval` as a constant on the endpoint, not as a function of data.
 */
export function pollInterval(items: ReadonlyArray<Task> | undefined): number {
  return items?.some((task) => TRANSITIONAL.has(task.state)) === true ? 5000 : 30000
}

/**
 * transitional are the states that mean work is still going on. They are the
 * reason the task list polls: an operator leaves this page open while an import
 * runs, and 30 s of staleness would be a lie about what the node is doing.
 */
const TRANSITIONAL: ReadonlySet<string> = new Set<TaskState>(['pending', 'running'])

export interface ListTasksArgs {
  /** Only tasks for this resource, e.g. an image id. */
  resourceId?: string
  /** Only tasks in one state. */
  state?: TaskState
}

/**
 * The task endpoints (api/openapi.yaml §/tasks).
 *
 * They live in their own module rather than under `features/images`, because the
 * instances pages will need them for the same reason: a launch is a task, and
 * the detail page shows its outcome.
 */
export const tasksApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listTasks: build.query<TaskList, ListTasksArgs | void>({
      query: (args) => ({
        url: '/tasks',
        params: {
          page_size: MAX_PAGE_SIZE,
          ...(args?.resourceId ? { resource_id: args.resourceId } : {}),
          ...(args?.state ? { state: args.state } : {}),
        },
      }),
      providesTags: ['Task'],
    }),

    getTask: build.query<Task, string>({
      query: (id) => ({ url: `/tasks/${encodeURIComponent(id)}` }),
      providesTags: (_result, _error, id) => [{ type: 'Task', id }],
    }),
  }),
})

export const { useListTasksQuery, useGetTaskQuery } = tasksApi
