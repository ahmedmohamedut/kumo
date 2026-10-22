import Box from '@cloudscape-design/components/box'
import ColumnLayout from '@cloudscape-design/components/column-layout'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Header from '@cloudscape-design/components/header'
import Link from '@cloudscape-design/components/link'
import ProgressBar from '@cloudscape-design/components/progress-bar'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import StatusIndicator from '@cloudscape-design/components/status-indicator'

import { usePageMeta } from '@/app/pageMeta'
import { useAppSelector } from '@/app/store'
import {
  instancesApi,
  pollInterval as instancePollInterval,
  useListInstancesQuery,
  type Instance,
  type InstanceState,
} from '@/features/instances/api'
import { useSystemCapacityQuery, type CapacityMetric } from '@/features/system/api'
import {
  pollInterval as taskPollInterval,
  tasksApi,
  useListTasksQuery,
  type Task,
} from '@/features/tasks/api'
import { taskIndicatorType, taskLabel } from '@/features/tasks/state'
import { formatBytes, formatRelativeTime } from '@/lib/format'

/** RECENT_TASKS is how many of the newest tasks the page lists. */
const RECENT_TASKS = 5

/**
 * The instance tallies the page shows, in a fixed order.
 *
 * Fixed rather than derived from the data: a row of tiles that appeared and
 * disappeared as states came and went would reflow under an operator watching a
 * boot, and "no instances are in error" is worth reading as a zero rather than
 * inferring from an absent tile.
 *
 * The transitional states are summed into one tile. An operator does not act
 * differently on `pending` than on `stopping` from here — both mean the node is
 * busy and the number will change on its own — and the instances table is one
 * click away when they want the detail.
 */
const TALLIES: ReadonlyArray<{
  id: string
  label: string
  states: ReadonlyArray<InstanceState>
  /**
   * The StatusIndicator the tile's label wears, or null for a plain count.
   * Status colours are reserved for state and always ship with their icon and
   * word, never as colour alone.
   */
  indicator: 'success' | 'stopped' | 'in-progress' | 'error' | null
}> = [
  { id: 'total', label: 'Instances', states: [], indicator: null },
  { id: 'running', label: 'Running', states: ['running'], indicator: 'success' },
  { id: 'stopped', label: 'Stopped', states: ['stopped'], indicator: 'stopped' },
  {
    id: 'in-progress',
    label: 'In progress',
    states: ['pending', 'stopping', 'terminating'],
    indicator: 'in-progress',
  },
  { id: 'error', label: 'Error', states: ['error'], indicator: 'error' },
]

/**
 * DashboardPage is what `/` shows: what this node is running, what it has handed
 * out, and what it has been doing (03-frontend.md §6, "counts of instances by
 * state, recent tasks, capacity bars").
 *
 * Everything here is a tally, a meter or a short list — no chart. The data has
 * no series to plot: the state counts are five numbers an operator reads
 * individually, and capacity is three magnitudes against a known total, which is
 * exactly what a meter is for. A pie of five states would answer a question
 * nobody asked ("what share of my instances are stopped?") less legibly than
 * the numbers themselves.
 */
export function DashboardPage() {
  usePageMeta({ breadcrumbs: [{ text: 'Dashboard', href: '/' }] })

  // Both lists poll, and both follow their own data: 5 s while anything is
  // moving, 30 s once the node is quiet (03-frontend.md §5). RTK Query types
  // `pollingInterval` as a constant on the endpoint, so the interval is read off
  // the cached copy of the query rather than computed in the hook call.
  const cachedInstances = useAppSelector(instancesApi.endpoints.listInstances.select())
  const { data: instanceData, isLoading: loadingInstances } = useListInstancesQuery(undefined, {
    pollingInterval: instancePollInterval(cachedInstances.data?.items),
  })

  const cachedTasks = useAppSelector(tasksApi.endpoints.listTasks.select())
  const { data: taskData } = useListTasksQuery(undefined, {
    pollingInterval: taskPollInterval(cachedTasks.data?.items),
  })

  // Capacity moves when an instance is launched or terminated, which the
  // instance poll already notices, so it rides the same interval.
  const { data: capacity, isLoading: loadingCapacity } = useSystemCapacityQuery(undefined, {
    pollingInterval: instancePollInterval(cachedInstances.data?.items),
  })

  const instances = instanceData?.items ?? []
  const tasks = (taskData?.items ?? []).slice(0, RECENT_TASKS)

  return (
    <ContentLayout
      header={
        <Header variant="h1" description="What this node is running right now">
          Dashboard
        </Header>
      }
    >
      <SpaceBetween size="l">
        <Container
          header={
            <Header
              variant="h2"
              actions={
                <Link href="/instances" variant="primary">
                  View all instances
                </Link>
              }
            >
              Instances
            </Header>
          }
        >
          {loadingInstances && instanceData === undefined ? (
            <Loading label="Loading the instances" />
          ) : (
            <InstanceTallies instances={instances} />
          )}
        </Container>

        <Container header={<Header variant="h2">Capacity</Header>}>
          {loadingCapacity && capacity === undefined ? (
            <Loading label="Loading the capacity" />
          ) : capacity === undefined ? (
            <Box color="text-status-inactive">The node did not report its capacity.</Box>
          ) : (
            <SpaceBetween size="l">
              <CapacityBar label="vCPUs" metric={capacity.vcpus} format={formatCount} />
              <CapacityBar label="Memory" metric={capacity.memory_bytes} format={formatBytes} />
              <CapacityBar label="Disk" metric={capacity.disk_bytes} format={formatBytes} />
            </SpaceBetween>
          )}
        </Container>

        <Container header={<Header variant="h2">Recent tasks</Header>}>
          {tasks.length === 0 ? (
            <Box color="text-status-inactive">Nothing has run on this node yet.</Box>
          ) : (
            <div data-testid="recent-tasks">
              <SpaceBetween size="m">
                {tasks.map((task) => (
                  <RecentTask key={task.id} task={task} />
                ))}
              </SpaceBetween>
            </div>
          )}
        </Container>
      </SpaceBetween>
    </ContentLayout>
  )
}

/** Loading is the spinner each panel shows on the very first render. */
function Loading({ label }: { label: string }) {
  return (
    <Box textAlign="center" padding="m">
      {/* A spinner with no name is invisible to a screen reader. */}
      <div role="status" aria-label={label}>
        <Spinner />
      </div>
    </Box>
  )
}

/** InstanceTallies is the row of counts, or the empty state on a fresh node. */
function InstanceTallies({ instances }: { instances: ReadonlyArray<Instance> }) {
  // `terminated` rows are leftovers from the release that kept terminated
  // instances as history; a terminate now deletes the row
  // (02-data-model.md). Counting them would overstate what the node runs, and
  // would leave the total above the sum of the tiles beside it — the same
  // reason the capacity report stops summing an instance once it is gone.
  const live = instances.filter((instance) => instance.state !== 'terminated')

  if (live.length === 0) {
    return (
      <SpaceBetween size="s">
        <Box color="text-status-inactive">
          No instances yet. Launching one is the quickest way to see this node work.
        </Box>
        <Link href="/instances/launch" variant="primary">
          Launch an instance
        </Link>
      </SpaceBetween>
    )
  }

  return (
    <ColumnLayout columns={TALLIES.length} variant="text-grid" minColumnWidth={120}>
      {TALLIES.map((tally) => {
        const count =
          tally.states.length === 0
            ? live.length
            : live.filter((instance) => tally.states.includes(instance.state)).length

        return (
          <div key={tally.id}>
            <Box variant="awsui-key-label">
              {tally.indicator === null ? (
                tally.label
              ) : (
                <StatusIndicator type={tally.indicator}>{tally.label}</StatusIndicator>
              )}
            </Box>
            {/* The number wears a text token, never a status colour: the
                indicator beside the label already carries that meaning, and a
                coloured figure would be colour-alone encoding. */}
            <Box fontSize="display-l" fontWeight="bold" data-testid={`tally-${tally.id}`}>
              {count}
            </Box>
          </div>
        )
      })}
    </ColumnLayout>
  )
}

/**
 * CapacityBar is one metric as a meter: what has been handed out against what
 * the node has (00-architecture.md §6).
 *
 * `allocated > total` is possible by design — vCPUs are offered at an overcommit
 * ratio, so a memory- or disk-bound node can be over-allocated — and the
 * endpoint reports that rather than hiding it. The bar cannot draw past full, so
 * it is clamped and the figure says what really happened; a meter silently
 * pinned at 100% would be the one number on this page an operator could not
 * trust.
 */
function CapacityBar({
  label,
  metric,
  format,
}: {
  label: string
  metric: CapacityMetric
  format: (value: number) => string
}) {
  const over = metric.total > 0 && metric.allocated > metric.total
  const percent = metric.total > 0 ? Math.round((metric.allocated / metric.total) * 100) : 0

  return (
    <ProgressBar
      value={Math.min(percent, 100)}
      status={over ? 'error' : 'in-progress'}
      label={label}
      description={`${format(metric.allocated)} of ${format(metric.total)} allocated`}
      additionalInfo={
        over
          ? `Over-allocated by ${format(metric.allocated - metric.total)}`
          : `${format(metric.total - metric.allocated)} left`
      }
      ariaLabel={`${label} allocated`}
    />
  )
}

/** RecentTask is one line of the recent list: what ran, how it went, and when. */
function RecentTask({ task }: { task: Task }) {
  return (
    <SpaceBetween size="xxs">
      <SpaceBetween direction="horizontal" size="s">
        <Box variant="code">{task.type}</Box>
        <StatusIndicator type={taskIndicatorType(task.state)}>
          {taskLabel(task.state)}
        </StatusIndicator>
      </SpaceBetween>

      <Box variant="small" color="text-status-inactive">
        {/* The error is what an operator needs when there is one; otherwise the
            resource and the age are enough to recognise the task. */}
        {task.error ??
          `${task.resource_id ?? 'this node'} · ${formatRelativeTime(task.created_at)}`}
      </Box>
    </SpaceBetween>
  )
}

/** formatCount renders a whole-number metric, which vCPUs are. */
function formatCount(value: number): string {
  return String(value)
}
