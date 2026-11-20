import Box from '@cloudscape-design/components/box'
import Link from '@cloudscape-design/components/link'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import type { Cluster } from '@/features/kubernetes/api'
import { CLUSTER_INDICATOR, readyCount } from '@/features/kubernetes/state'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/** The columns of the clusters table. */
export const columnDefinitions: ReadonlyArray<TableProps.ColumnDefinition<Cluster>> = [
  {
    id: 'name',
    header: 'Name',
    sortingField: 'name',
    isRowHeader: true,
    cell: (item) => <Link href={`/kubernetes/${item.id}`}>{item.name}</Link>,
  },
  {
    id: 'state',
    header: 'Status',
    sortingField: 'state',
    cell: (item) => (
      <StatusIndicator type={CLUSTER_INDICATOR[item.state].type}>
        {CLUSTER_INDICATOR[item.state].label}
      </StatusIndicator>
    ),
  },
  {
    id: 'version',
    header: 'Kubernetes',
    sortingField: 'kubernetes_version',
    cell: (item) => item.kubernetes_version,
  },
  {
    id: 'nodes',
    header: 'Nodes Ready',
    cell: (item) => {
      const { ready, total } = readyCount(item)
      return `${ready} / ${total}`
    },
  },
  {
    id: 'endpoint',
    header: 'Endpoint',
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
