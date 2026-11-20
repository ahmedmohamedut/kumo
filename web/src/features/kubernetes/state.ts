import type { StatusIndicatorProps } from '@cloudscape-design/components'

import type { Cluster, ClusterState } from '@/features/kubernetes/api'

/**
 * State → StatusIndicator mapping for clusters (03-frontend.md §6.10).
 * `degraded` is a warning, not an error: the control plane answers, and the
 * reason says what is wrong.
 */
export const CLUSTER_INDICATOR: Record<
  ClusterState,
  { type: StatusIndicatorProps.Type; label: string }
> = {
  creating: { type: 'in-progress', label: 'Creating' },
  active: { type: 'success', label: 'Active' },
  updating: { type: 'in-progress', label: 'Updating' },
  degraded: { type: 'warning', label: 'Degraded' },
  deleting: { type: 'in-progress', label: 'Deleting' },
  failed: { type: 'error', label: 'Failed' },
}

/** Bounds of the create form and the resize, from 01-api.md §Kubernetes. */
export const MAX_NAME_LENGTH = 40
export const MIN_NODE_COUNT = 1
export const MAX_NODE_COUNT = 10
/** kubeadm's minimums: a control plane needs 2 vCPU, every node 2048 MiB. */
export const MIN_CONTROL_PLANE_VCPUS = 2
export const MIN_NODE_MEMORY_MIB = 2048

const NAME_RE = /^[a-z]([-a-z0-9]*[a-z0-9])?$/

/** nameProblem explains why a cluster name is refused locally, or null. */
export function nameProblem(name: string): string | null {
  if (name === '') {
    return 'Enter a name for the cluster.'
  }
  if (name.length > MAX_NAME_LENGTH) {
    return `Must be at most ${MAX_NAME_LENGTH} characters.`
  }
  if (!NAME_RE.test(name) || name.includes('--')) {
    return 'Must start with a letter and hold lowercase letters, digits and single dashes, and cannot end with a dash.'
  }
  return null
}

/** nodeCountProblem bounds a node group's size. */
export function nodeCountProblem(count: number): string | null {
  if (!Number.isInteger(count) || count < MIN_NODE_COUNT || count > MAX_NODE_COUNT) {
    return `Must be a whole number between ${MIN_NODE_COUNT} and ${MAX_NODE_COUNT}.`
  }
  return null
}

/** canScale: one scale at a time, once the cluster is up. */
export function canScale(state: ClusterState): boolean {
  return state === 'active' || state === 'degraded'
}

/** canDownloadKubeconfig: the API mints one once the cluster is up. */
export function canDownloadKubeconfig(state: ClusterState): boolean {
  return state === 'active' || state === 'degraded' || state === 'updating'
}

/** canDelete: every state but `deleting`, because delete is also the way out. */
export function canDelete(state: ClusterState): boolean {
  return state !== 'deleting'
}

/** kubectlExample is what the Connect tab offers once a kubeconfig is saved. */
export function kubectlExample(cluster: Cluster): string {
  return `KUBECONFIG=./${cluster.name}.kubeconfig kubectl get nodes`
}

/** readyCount is "how many of the nodes the last look saw Ready". */
export function readyCount(cluster: Cluster): { ready: number; total: number } {
  return {
    ready: cluster.nodes.filter((node) => node.ready === true).length,
    total: cluster.nodes.length,
  }
}
