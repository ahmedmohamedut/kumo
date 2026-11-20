import { kumoApi } from '@/app/api'
import type { components } from '@/types/api'

export type Cluster = components['schemas']['Cluster']
export type ClusterState = components['schemas']['ClusterState']
export type ClusterNode = components['schemas']['ClusterNode']
export type NodeGroup = components['schemas']['NodeGroup']
export type CreateClusterRequest = components['schemas']['CreateClusterRequest']
export type Kubeconfig = components['schemas']['Kubeconfig']
type ClusterList = components['schemas']['ClusterList']

/** The states a task is working on, which the pages poll until they settle. */
const TRANSITIONAL: ReadonlySet<ClusterState> = new Set<ClusterState>([
  'creating',
  'updating',
  'deleting',
])

/**
 * pollInterval keeps a list or detail page fresh while anything is moving. A
 * create takes a few minutes -- a control plane and every worker booting and
 * joining -- so the page follows it at 5 s and drops to 30 s once settled.
 * `degraded` is polled at 5 s too: the health pass brings a cluster back to
 * `active` on its own, and that is what an operator watching it waits for.
 */
export function pollInterval(items: ReadonlyArray<Cluster> | undefined): number {
  if (items === undefined) {
    return 30000
  }
  return items.some((item) => TRANSITIONAL.has(item.state) || item.state === 'degraded')
    ? 5000
    : 30000
}

/**
 * The Kubernetes endpoints (api/openapi.yaml §/eks). Every one answers `503`
 * on a node with the module off; the pages read that from `/system/info`
 * first. Creating, scaling and deleting invalidate `Instance` as well, because
 * a cluster's nodes hold the vCPUs, memory and disk `/system/capacity` reports.
 */
export const clustersApi = kumoApi.injectEndpoints({
  endpoints: (build) => ({
    listClusters: build.query<ClusterList, void>({
      query: () => ({ url: '/eks/clusters', params: { page_size: 200 } }),
      providesTags: ['Cluster'],
    }),

    getCluster: build.query<Cluster, string>({
      query: (id) => ({ url: `/eks/clusters/${id}` }),
      providesTags: (_result, _error, id) => [{ type: 'Cluster', id }],
    }),

    createCluster: build.mutation<Cluster, CreateClusterRequest>({
      query: (body) => ({ url: '/eks/clusters', method: 'POST', body }),
      invalidatesTags: ['Cluster', 'Task', 'Instance'],
    }),

    scaleNodeGroup: build.mutation<
      Cluster,
      { clusterId: string; nodeGroupId: string; desiredSize: number }
    >({
      query: ({ clusterId, nodeGroupId, desiredSize }) => ({
        url: `/eks/clusters/${clusterId}/node-groups/${nodeGroupId}`,
        method: 'PATCH',
        body: { desired_size: desiredSize },
      }),
      invalidatesTags: (_result, _error, { clusterId }) => [
        { type: 'Cluster', id: clusterId },
        'Cluster',
        'Task',
        'Instance',
      ],
    }),

    deleteCluster: build.mutation<Cluster, string>({
      query: (id) => ({ url: `/eks/clusters/${id}`, method: 'DELETE' }),
      invalidatesTags: (_result, _error, id) => [
        { type: 'Cluster', id },
        'Cluster',
        'Task',
        'Instance',
      ],
    }),

    // A mutation, not a query: every call signs a fresh certificate and the
    // answer holds its private key, which must not sit in the query cache.
    mintKubeconfig: build.mutation<Kubeconfig, string>({
      query: (id) => ({ url: `/eks/clusters/${id}/kubeconfig`, method: 'POST' }),
    }),
  }),
})

export const {
  useListClustersQuery,
  useGetClusterQuery,
  useCreateClusterMutation,
  useScaleNodeGroupMutation,
  useDeleteClusterMutation,
  useMintKubeconfigMutation,
} = clustersApi
