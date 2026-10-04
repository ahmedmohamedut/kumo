import Box from '@cloudscape-design/components/box'
import Link from '@cloudscape-design/components/link'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import { ImageStateIndicator } from '@/features/images/ImageStateIndicator'
import type { Image } from '@/features/images/state'
import { formatAbsoluteTime, formatBytes, formatGiB, formatRelativeTime } from '@/lib/format'

export type { Image }

/**
 * The default column set of the images table (03-frontend.md §6.4): Name, ID,
 * OS, Architecture, State, Size, Created.
 *
 * Two of these carry more than the obvious value:
 *
 * - **State** renders the reason and the progress bar, because a table that says
 *   only "Failed" sends the operator looking in the wrong place.
 * - **OS** is `os_family os_version`, and falls back to the source URL's host
 *   when Kumo has no metadata: an imported image with no OS filled in is normal
 *   (the field is optional), and an empty cell would read as broken.
 */
export const defaultColumns: ReadonlyArray<TableProps.ColumnDefinition<Image>> = [
  {
    id: 'name',
    header: 'Name',
    sortingField: 'name',
    cell: (item) => <Link href={`/images/${item.id}`}>{item.name}</Link>,
  },
  {
    id: 'id',
    header: 'Image ID',
    sortingField: 'id',
    cell: (item) => <Box variant="code">{item.id}</Box>,
  },
  {
    id: 'os',
    header: 'OS',
    sortingField: 'os_family',
    cell: (item) => osCell(item),
  },
  {
    id: 'architecture',
    header: 'Architecture',
    sortingField: 'architecture',
    cell: (item) => (item.architecture === 'aarch64' ? 'arm64' : 'x86_64'),
  },
  {
    id: 'state',
    header: 'State',
    sortingField: 'state',
    cell: (item) => <ImageStateIndicator image={item} />,
  },
  {
    id: 'size',
    header: 'Size',
    cell: (item) => sizeCell(item),
  },
  {
    id: 'min_disk_gb',
    header: 'Min disk',
    // No `?? 0`: an image with no recorded minimum disk is unknown, and "0 GiB"
    // would be a claim that no root disk can boot from it.
    cell: (item) => formatGiB(item.min_disk_gb),
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

/** osCell is the OS column: metadata when known, the source host otherwise. */
function osCell(item: Image) {
  const parts = [item.os_family, item.os_version].filter(
    (part) => part !== undefined && part !== '',
  )
  if (parts.length > 0) {
    return parts.join(' ')
  }
  return hostOf(item.source_url) ?? '—'
}

/**
 * sizeCell is the size on disk. It is a dash while an import is still running,
 * because 0 B would read as an empty image rather than an unfinished one.
 */
function sizeCell(item: Image) {
  return formatBytes(item.size_bytes)
}

/** hostOf is the hostname of a source URL, or null when there is none to show. */
function hostOf(url: string | null | undefined): string | null {
  if (url === null || url === undefined || url === '') {
    return null
  }
  try {
    return new URL(url).host || null
  } catch {
    return null
  }
}

/** columnDisplay starts with every column visible, in column definition order. */
export const columnDisplayPreferences: ReadonlyArray<TableProps.ColumnDisplayProperties> = [
  { id: 'name', visible: true },
  { id: 'id', visible: true },
  { id: 'os', visible: true },
  { id: 'architecture', visible: true },
  { id: 'state', visible: true },
  { id: 'size', visible: true },
  { id: 'min_disk_gb', visible: true },
  { id: 'created_at', visible: true },
]

/**
 * columnOptions is the column list in the preferences panel. `contentDisplayPreference`
 * takes a flat list (only the cards-flavoured `visibleContentPreference` nests), and the
 * ids must match `columnDisplay` or toggling a column there does nothing.
 */
export const columnOptions: ReadonlyArray<CollectionPreferencesProps.ContentDisplayOption> = [
  { id: 'name', label: 'Name' },
  { id: 'id', label: 'Image ID' },
  { id: 'os', label: 'OS' },
  { id: 'architecture', label: 'Architecture' },
  { id: 'state', label: 'State' },
  { id: 'size', label: 'Size' },
  { id: 'min_disk_gb', label: 'Min disk' },
  { id: 'created_at', label: 'Created' },
]
