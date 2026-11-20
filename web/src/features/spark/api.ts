import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type SparkCluster = components['schemas']['SparkCluster']
export type SparkClusterState = components['schemas']['ClusterState']
export type SparkNode = components['schemas']['SparkNode']
export type SparkNodeGroup = components['schemas']['NodeGroup']
export type CreateSparkClusterRequest = components['schemas']['CreateSparkClusterRequest']
export type SparkClientConfig = components['schemas']['SparkClientConfig']
type SparkClusterList = components['schemas']['SparkClusterList']

const TRANSITIONAL: ReadonlySet<SparkClusterState> = new Set<SparkClusterState>([
  'creating',
  'updating',
  'deleting',
])

/**
 * pollInterval keeps a page fresh while anything is moving: 5 s while a
 * cluster is transitional or degraded (the health pass recovers it on its
 * own), 30 s once settled.
 */
export function pollInterval(items: ReadonlyArray<SparkCluster> | undefined): number {
  if (items === undefined) {
    return 30000
  }
  return items.some((item) => TRANSITIONAL.has(item.state) || item.state === 'degraded')
    ? 5000
    : 30000
}

/**
 * The Spark endpoints (api/openapi.yaml §/spark). Every one answers `503` on a
 * node with the module off; the pages read that from `/system/info` first.
 * Creating, scaling and deleting invalidate `Instance` as well, because a
 * cluster's nodes hold capacity `/system/capacity` reports.
 */
export const sparkApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listSparkClusters: build.query<SparkClusterList, void>({
      query: () => ({ url: '/spark/clusters', params: { page_size: 200 } }),
      providesTags: ['SparkCluster'],
    }),

    getSparkCluster: build.query<SparkCluster, string>({
      query: (id) => ({ url: `/spark/clusters/${id}` }),
      providesTags: (_result, _error, id) => [{ type: 'SparkCluster', id }],
    }),

    createSparkCluster: build.mutation<SparkCluster, CreateSparkClusterRequest>({
      query: (body) => ({ url: '/spark/clusters', method: 'POST', body }),
      invalidatesTags: ['SparkCluster', 'Task', 'Instance'],
    }),

    scaleSparkNodeGroup: build.mutation<
      SparkCluster,
      { clusterId: string; nodeGroupId: string; desiredSize: number }
    >({
      query: ({ clusterId, nodeGroupId, desiredSize }) => ({
        url: `/spark/clusters/${clusterId}/node-groups/${nodeGroupId}`,
        method: 'PATCH',
        body: { desired_size: desiredSize },
      }),
      invalidatesTags: (_result, _error, { clusterId }) => [
        { type: 'SparkCluster', id: clusterId },
        'SparkCluster',
        'Task',
        'Instance',
      ],
    }),

    deleteSparkCluster: build.mutation<SparkCluster, string>({
      query: (id) => ({ url: `/spark/clusters/${id}`, method: 'DELETE' }),
      invalidatesTags: (_result, _error, id) => [
        { type: 'SparkCluster', id },
        'SparkCluster',
        'Task',
        'Instance',
      ],
    }),

    // A mutation, not a query: the answer carries the cluster's secret and S3
    // key, which must not sit in the query cache.
    sparkClientConfig: build.mutation<SparkClientConfig, string>({
      query: (id) => ({ url: `/spark/clusters/${id}/client-config`, method: 'POST' }),
    }),
  }),
})

export const {
  useListSparkClustersQuery,
  useGetSparkClusterQuery,
  useCreateSparkClusterMutation,
  useScaleSparkNodeGroupMutation,
  useDeleteSparkClusterMutation,
  useSparkClientConfigMutation,
} = sparkApi
