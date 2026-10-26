import Link from '@cloudscape-design/components/link'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import Box from '@cloudscape-design/components/box'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import type { Bucket } from '@/features/buckets/api'
import { BUCKET_INDICATOR } from '@/features/buckets/state'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/**
 * The columns of the buckets table. Versioning is not among them: the list
 * endpoint returns plain buckets, and the versioning state takes one request
 * per bucket, so it lives on the detail page.
 */
export const columnDefinitions: ReadonlyArray<TableProps.ColumnDefinition<Bucket>> = [
  {
    id: 'name',
    header: 'Name',
    sortingField: 'name',
    isRowHeader: true,
    cell: (item) => <Link href={`/buckets/${encodeURIComponent(item.name)}`}>{item.name}</Link>,
  },
  {
    id: 'state',
    header: 'State',
    sortingField: 'state',
    cell: (item) => (
      <StatusIndicator type={BUCKET_INDICATOR[item.state].type}>
        {BUCKET_INDICATOR[item.state].label}
      </StatusIndicator>
    ),
  },
  {
    id: 'created_by',
    header: 'Created by',
    sortingComparator: (a, b) => (a.created_by ?? '').localeCompare(b.created_by ?? ''),
    // A bucket made with another client has no creator Kumo knows of.
    cell: (item) =>
      item.created_by ?? (
        <Box variant="span" color="text-status-inactive">
          Outside Kumo
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
  {
    id: 'tags',
    header: 'Tags',
    cell: (item) => {
      const count = Object.keys(item.tags).length
      return count === 0 ? '—' : `${count} tag${count === 1 ? '' : 's'}`
    },
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
