import Box from '@cloudscape-design/components/box'
import Link from '@cloudscape-design/components/link'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import { InstanceStateIndicator } from '@/features/instances/InstanceStateIndicator'
import type { Instance } from '@/features/instances/state'
import { formatAbsoluteTime, formatGiB, formatRelativeTime } from '@/lib/format'

export type { Instance }

/**
 * The default column set of the instances table (03-frontend.md §6.1): Name,
 * Instance ID, State, Instance type, Private IP, Image, Key pair, Launched.
 *
 * Three of these carry more than the obvious value:
 *
 * - **State** renders the reason, because "Error" on its own tells an operator
 *   nothing about whether to resize the disk, re-import the image or look at
 *   libvirt.
 * - **Image** and **Key pair** use the denormalised names the API carries. The
 *   list endpoint hands them over precisely so this table does not have to join
 *   against two other collections per row.
 * - **Private IP** is a dash while there is none, rather than an empty cell: an
 *   instance that has not booted yet genuinely has no address, and blank would
 *   read as a rendering fault.
 */
export const defaultColumns: ReadonlyArray<TableProps.ColumnDefinition<Instance>> = [
  {
    id: 'name',
    header: 'Name',
    sortingField: 'name',
    cell: (item) => <Link href={`/instances/${item.id}`}>{item.name}</Link>,
  },
  {
    id: 'id',
    header: 'Instance ID',
    sortingField: 'id',
    cell: (item) => <Box variant="code">{item.id}</Box>,
  },
  {
    id: 'state',
    header: 'State',
    sortingField: 'state',
    cell: (item) => <InstanceStateIndicator instance={item} />,
  },
  {
    id: 'instance_type',
    header: 'Instance type',
    sortingField: 'instance_type',
    cell: (item) => item.instance_type,
  },
  {
    id: 'private_ip',
    header: 'Private IP',
    cell: (item) => item.private_ip ?? '—',
  },
  {
    id: 'image_name',
    header: 'Image',
    sortingField: 'image_name',
    cell: (item) => item.image_name ?? item.image_id,
  },
  {
    id: 'key_pair_name',
    header: 'Key pair',
    cell: (item) => item.key_pair_name ?? '—',
  },
  {
    id: 'root_disk_gb',
    header: 'Root disk',
    cell: (item) => formatGiB(item.root_disk_gb),
  },
  {
    id: 'launched_at',
    header: 'Launched',
    sortingField: 'launched_at',
    cell: (item) => (
      <span title={formatAbsoluteTime(item.launched_at)}>
        {formatRelativeTime(item.launched_at)}
      </span>
    ),
  },
]

/** columnDisplay starts with every column visible, in column definition order. */
export const columnDisplayPreferences: ReadonlyArray<TableProps.ColumnDisplayProperties> = [
  { id: 'name', visible: true },
  { id: 'id', visible: true },
  { id: 'state', visible: true },
  { id: 'instance_type', visible: true },
  { id: 'private_ip', visible: true },
  { id: 'image_name', visible: true },
  { id: 'key_pair_name', visible: true },
  { id: 'root_disk_gb', visible: true },
  { id: 'launched_at', visible: true },
]

/**
 * columnOptions is the column list in the preferences panel. `contentDisplayPreference`
 * takes a flat list (only the cards-flavoured `visibleContentPreference` nests), and the
 * ids must match `columnDisplay` or toggling a column there does nothing.
 */
export const columnOptions: ReadonlyArray<CollectionPreferencesProps.ContentDisplayOption> = [
  { id: 'name', label: 'Name' },
  { id: 'id', label: 'Instance ID' },
  { id: 'state', label: 'State' },
  { id: 'instance_type', label: 'Instance type' },
  { id: 'private_ip', label: 'Private IP' },
  { id: 'image_name', label: 'Image' },
  { id: 'key_pair_name', label: 'Key pair' },
  { id: 'root_disk_gb', label: 'Root disk' },
  { id: 'launched_at', label: 'Launched' },
]
