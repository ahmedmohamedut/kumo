import Box from '@cloudscape-design/components/box'
import Link from '@cloudscape-design/components/link'
import type { CollectionPreferencesProps, TableProps } from '@cloudscape-design/components'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'
import type { components } from '@/types/api'

/** KeyPair is the API shape of one row. */
export type KeyPair = components['schemas']['KeyPair']

/**
 * The default column set of the key pairs table (03-frontend.md §6.5): Name, ID,
 * Type, Fingerprint, Created.
 *
 * The ID is a link even though there is no detail page in this milestone: it
 * points at the list filtered to that name, so the column stays honest and the
 * operator can still get back to one key. The fingerprint is the value an
 * operator pastes into `ssh-keygen -lf`, so it is selectable text and never
 * truncated behind a hover.
 */
export const defaultColumns: ReadonlyArray<TableProps.ColumnDefinition<KeyPair>> = [
  {
    id: 'name',
    header: 'Name',
    sortingField: 'name',
    cell: (item: KeyPair) => (
      <Link href={`/key-pairs?q=${encodeURIComponent(item.name)}`}>{item.name}</Link>
    ),
  },
  {
    id: 'id',
    header: 'Key pair ID',
    sortingField: 'id',
    cell: (item: KeyPair) => <Box variant="code">{item.id}</Box>,
  },
  {
    id: 'type',
    header: 'Type',
    sortingField: 'type',
    cell: (item: KeyPair) => (item.type === 'rsa' ? 'RSA' : 'ED25519'),
  },
  {
    id: 'fingerprint',
    header: 'Fingerprint',
    cell: (item: KeyPair) => <Box variant="code">{item.fingerprint}</Box>,
  },
  {
    id: 'created_at',
    header: 'Created',
    sortingField: 'created_at',
    cell: (item: KeyPair) => (
      <span title={formatAbsoluteTime(item.created_at)}>{formatRelativeTime(item.created_at)}</span>
    ),
  },
]

/** columnDisplay starts with every column visible, in column definition order. */
export const columnDisplayPreferences: ReadonlyArray<TableProps.ColumnDisplayProperties> = [
  { type: 'column', id: 'name', visible: true },
  { type: 'column', id: 'id', visible: true },
  { type: 'column', id: 'type', visible: true },
  { type: 'column', id: 'fingerprint', visible: true },
  { type: 'column', id: 'created_at', visible: true },
]

/**
 * columnOptions is the column list in the preferences panel. `contentDisplayPreference`
 * takes a flat list (only the cards-flavoured `visibleContentPreference` nests), and
 * the ids must match `columnDisplay` or toggling a column there does nothing.
 */
export const columnOptions: ReadonlyArray<CollectionPreferencesProps.ContentDisplayOption> = [
  { id: 'name', label: 'Name' },
  { id: 'id', label: 'Key pair ID' },
  { id: 'type', label: 'Type' },
  { id: 'fingerprint', label: 'Fingerprint' },
  { id: 'created_at', label: 'Created' },
]
