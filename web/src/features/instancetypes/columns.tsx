import Box from '@cloudscape-design/components/box'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'

import type { InstanceType } from '@/features/instancetypes/api'
import { formatMemoryMiB } from '@/lib/format'

/**
 * The column set of the instance types table: Name, vCPUs, Memory,
 * Description. The id *is* the name of a type (`kumo.small`), so there is no
 * separate ID column. There is no detail page either; everything a type has
 * fits in a row, and the instances using it are shown where they matter, in
 * the delete dialog.
 */
export const columnDefinitions: ReadonlyArray<TableProps.ColumnDefinition<InstanceType>> = [
  {
    id: 'id',
    header: 'Name',
    sortingField: 'id',
    isRowHeader: true,
    cell: (item) => <Box variant="code">{item.id}</Box>,
  },
  {
    id: 'vcpus',
    header: 'vCPUs',
    sortingField: 'vcpus',
    cell: (item) => item.vcpus,
  },
  {
    id: 'memory_mib',
    header: 'Memory',
    sortingField: 'memory_mib',
    cell: (item) => formatMemoryMiB(item.memory_mib),
  },
  {
    id: 'description',
    header: 'Description',
    cell: (item) => item.description || '—',
  },
]

/** columnDisplay starts with every column visible, in definition order. */
export const columnDisplay: ReadonlyArray<TableProps.ColumnDisplayProperties> = [
  { id: 'id', visible: true },
  { id: 'vcpus', visible: true },
  { id: 'memory_mib', visible: true },
  { id: 'description', visible: true },
]

/** columnOptions is the column list of the preferences panel. */
export const columnOptions: ReadonlyArray<CollectionPreferencesProps.ContentDisplayOption> = [
  { id: 'id', label: 'Name', alwaysVisible: true },
  { id: 'vcpus', label: 'vCPUs' },
  { id: 'memory_mib', label: 'Memory' },
  { id: 'description', label: 'Description' },
]
