import Box from '@cloudscape-design/components/box'
import Link from '@cloudscape-design/components/link'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import type { SparkCluster } from '@/features/spark/api'
import { aliveCount, SPARK_INDICATOR } from '@/features/spark/state'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/** The columns of the Spark clusters table. */
export const columnDefinitions: ReadonlyArray<TableProps.ColumnDefinition<SparkCluster>> = [
  {
    id: 'name',
    header: 'Name',
    sortingField: 'name',
    isRowHeader: true,
    cell: (item) => <Link href={`/spark/${item.id}`}>{item.name}</Link>,
  },
  {
    id: 'state',
    header: 'Status',
    sortingField: 'state',
    cell: (item) => (
      <StatusIndicator type={SPARK_INDICATOR[item.state].type}>
        {SPARK_INDICATOR[item.state].label}
      </StatusIndicator>
    ),
  },
  {
    id: 'version',
    header: 'Spark',
    sortingField: 'spark_version',
    cell: (item) => item.spark_version,
  },
  {
    id: 'workers',
    header: 'Workers alive',
    cell: (item) => {
      const { alive, total } = aliveCount(item)
      return `${alive} / ${total}`
    },
  },
  {
    id: 'storage',
    header: 'Object storage',
    cell: (item) =>
      item.object_storage ? (
        item.buckets.join(', ')
      ) : (
        <Box variant="span" color="text-status-inactive">
          —
        </Box>
      ),
  },
  {
    id: 'endpoint',
    header: 'Master',
    cell: (item) =>
      item.endpoint ?? (
        <Box variant="span" color="text-status-inactive">
          —
        </Box>
      ),
  },
  {
    id: 'created_at',
    header: 'Created',
    sortingField: 'created_at',
    cell: (item) => (
      <span title={formatAbsoluteTime(item.created_at)}>{formatRelativeTime(item.created_at)}</span>
    ),
  },
]

export const columnDisplay: ReadonlyArray<CollectionPreferencesProps.ContentDisplayItem> =
  columnDefinitions.map((column) => ({ id: column.id ?? '', visible: true }))

export const columnOptions: ReadonlyArray<CollectionPreferencesProps.ContentDisplayOption> =
  columnDefinitions.map((column) => ({
    id: column.id ?? '',
    label: String(column.header),
    alwaysVisible: column.id === 'name',
  }))
