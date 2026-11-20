import type { StatusIndicatorProps } from '@cloudscape-design/components'

import type { SparkCluster, SparkClusterState } from '@/features/spark/api'

/**
 * State → StatusIndicator mapping for Spark clusters (03-frontend.md §6.11),
 * the Kubernetes one: `degraded` is a warning, because the master answers.
 */
export const SPARK_INDICATOR: Record<
  SparkClusterState,
  { type: StatusIndicatorProps.Type; label: string }
> = {
  creating: { type: 'in-progress', label: 'Creating' },
  active: { type: 'success', label: 'Active' },
  updating: { type: 'in-progress', label: 'Updating' },
  degraded: { type: 'warning', label: 'Degraded' },
  deleting: { type: 'in-progress', label: 'Deleting' },
  failed: { type: 'error', label: 'Failed' },
}

/** Bounds of the create form and the resize, from 01-api.md §Spark. */
export const MAX_NAME_LENGTH = 40
export const MIN_WORKERS = 1
export const MAX_WORKERS = 10
export const MAX_BUCKETS = 10
/** A master runs the master and the history server; a worker its executors. */
export const MIN_MASTER_VCPUS = 2
export const MIN_MASTER_MEMORY_MIB = 4096
export const MIN_WORKER_MEMORY_MIB = 2048

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

/** workerCountProblem bounds a node group's size. */
export function workerCountProblem(count: number): string | null {
  if (!Number.isInteger(count) || count < MIN_WORKERS || count > MAX_WORKERS) {
    return `Must be a whole number between ${MIN_WORKERS} and ${MAX_WORKERS}.`
  }
  return null
}

/** bucketsProblem checks the object storage choices as the API does. */
export function bucketsProblem(buckets: string[], eventLogBucket: string): string | null {
  if (buckets.length === 0) {
    return 'Choose at least one bucket the cluster may use.'
  }
  if (buckets.length > MAX_BUCKETS) {
    return `Choose at most ${MAX_BUCKETS} buckets.`
  }
  if (eventLogBucket !== '' && !buckets.includes(eventLogBucket)) {
    return 'The event-log bucket must be one of the buckets.'
  }
  return null
}

export function canScale(state: SparkClusterState): boolean {
  return state === 'active' || state === 'degraded'
}

/** canDownloadConfig: the API renders one once the cluster is up. */
export function canDownloadConfig(state: SparkClusterState): boolean {
  return state === 'active' || state === 'degraded' || state === 'updating'
}

export function canDelete(state: SparkClusterState): boolean {
  return state !== 'deleting'
}

/**
 * submitExample is what the Connect tab offers: the downloaded configuration,
 * and the driver advertising this instance's address, which every worker can
 * reach (09-spark.md section 5).
 */
export function submitExample(cluster: SparkCluster): string {
  return (
    `spark-submit --properties-file ./kumo-${cluster.id}.conf ` +
    `--conf spark.driver.host=$(hostname -I | cut -d' ' -f1) ` +
    `job.py`
  )
}

/** aliveCount is "how many workers the master last counted ALIVE". */
export function aliveCount(cluster: SparkCluster): { alive: number; total: number } {
  const workers = cluster.nodes.filter((node) => node.role === 'worker')
  return {
    alive: workers.filter((node) => node.alive === true).length,
    total: workers.length,
  }
}
