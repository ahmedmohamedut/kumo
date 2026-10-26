import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useCollection } from '@cloudscape-design/collection-hooks'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Header from '@cloudscape-design/components/header'
import Modal from '@cloudscape-design/components/modal'
import Pagination from '@cloudscape-design/components/pagination'
import SpaceBetween from '@cloudscape-design/components/space-between'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import Table, { type TableProps } from '@cloudscape-design/components/table'
import TextFilter from '@cloudscape-design/components/text-filter'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import {
  LEVEL_LABEL,
  useDeleteAccessKeyMutation,
  useListAccessKeysQuery,
  type AccessKey,
} from '@/features/accesskeys/api'
import { ObjectStorageGate } from '@/features/buckets/ObjectStorageGate'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

const columns: ReadonlyArray<TableProps.ColumnDefinition<AccessKey>> = [
  { id: 'name', header: 'Name', sortingField: 'name', isRowHeader: true, cell: (key) => key.name },
  {
    id: 'access_key',
    header: 'Access key',
    sortingField: 'access_key',
    cell: (key) => <Box variant="code">{key.access_key}</Box>,
  },
  { id: 'level', header: 'Access', sortingField: 'level', cell: (key) => LEVEL_LABEL[key.level] },
  { id: 'buckets', header: 'Buckets', cell: (key) => key.buckets.join(', ') },
  {
    id: 'state',
    header: 'State',
    sortingField: 'state',
    cell: (key) =>
      key.state === 'active' ? (
        <StatusIndicator type="success">Active</StatusIndicator>
      ) : (
        <StatusIndicator type="error">Missing from RustFS</StatusIndicator>
      ),
  },
  { id: 'created_by', header: 'Created by', cell: (key) => key.created_by ?? '—' },
  {
    id: 'created_at',
    header: 'Created',
    sortingField: 'created_at',
    cell: (key) => (
      <span title={formatAbsoluteTime(key.created_at)}>{formatRelativeTime(key.created_at)}</span>
    ),
  },
]

/** AccessKeysListPage is Storage → Access keys, behind the object storage gate. */
export function AccessKeysListPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Storage', href: '/buckets' },
      { text: 'Access keys', href: '/access-keys' },
    ],
  })
  return (
    <ObjectStorageGate title="Access keys">
      <AccessKeysTable />
    </ObjectStorageGate>
  )
}

/**
 * AccessKeysTable lists the S3 credentials Kumo issued. No row carries a
 * secret: it was shown once, when the key was created.
 */
function AccessKeysTable() {
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { data, isLoading, isFetching, error, refetch } = useListAccessKeysQuery()
  const [pendingDelete, setPendingDelete] = useState<AccessKey | null>(null)

  const keys = data ?? []
  const { items, actions, collectionProps, filterProps, filteredItemsCount, paginationProps } =
    useCollection(keys, {
      filtering: { fields: ['name', 'access_key', 'buckets'] },
      sorting: {},
      pagination: { pageSize: 20 },
      selection: { trackBy: 'access_key' },
    })
  const selected = collectionProps.selectedItems?.[0]

  return (
    <>
      <Table
        {...collectionProps}
        items={items}
        columnDefinitions={columns}
        loading={isLoading}
        loadingText="Loading access keys"
        selectionType={isAdmin ? 'single' : undefined}
        trackBy="access_key"
        variant="full-page"
        ariaLabels={{
          selectionGroupLabel: 'Access key selection',
          itemSelectionLabel: (_data, key) => `Select ${key.name}`,
        }}
        empty={
          keys.length === 0 ? (
            <Box textAlign="center" padding="l">
              <SpaceBetween size="s" alignItems="center">
                <Header variant="h2">No access keys</Header>
                <Box variant="p">
                  An access key lets an application use chosen buckets with any S3 client.
                </Box>
                {isAdmin && (
                  <Button onClick={() => navigate('/access-keys/create')}>Create access key</Button>
                )}
              </SpaceBetween>
            </Box>
          ) : (
            <Box textAlign="center" padding="l">
              <SpaceBetween size="s" alignItems="center">
                <Header variant="h2">No matches</Header>
                <Button onClick={() => actions.setFiltering('')}>Clear filter</Button>
              </SpaceBetween>
            </Box>
          )
        }
        header={
          <Header
            variant="awsui-h1-sticky"
            counter={keys.length > 0 ? `(${keys.length})` : undefined}
            description="S3 credentials for applications, each limited to chosen buckets."
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh access keys"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <Button
                  disabled={!isAdmin || selected === undefined || selected.state !== 'active'}
                  onClick={() =>
                    selected &&
                    navigate(`/access-keys/${encodeURIComponent(selected.access_key)}/edit`)
                  }
                >
                  Edit
                </Button>
                <Button
                  disabled={!isAdmin || selected === undefined}
                  onClick={() => selected && setPendingDelete(selected)}
                  data-testid="delete-access-key"
                >
                  Delete
                </Button>
                <Button
                  variant="primary"
                  disabled={!isAdmin}
                  onClick={() => navigate('/access-keys/create')}
                >
                  Create access key
                </Button>
              </SpaceBetween>
            }
          >
            Access keys
          </Header>
        }
        filter={
          <TextFilter
            {...filterProps}
            filteringAriaLabel="Filter access keys"
            filteringPlaceholder="Find access keys"
            countText={`${filteredItemsCount ?? 0} matches`}
          />
        }
        pagination={
          <Pagination
            {...paginationProps}
            ariaLabels={{ paginationLabel: 'Access key pagination' }}
          />
        }
      />

      {error !== undefined && (
        <Box padding="m">
          <Alert type="error" header="The access keys could not be loaded">
            {errorMessage(error, 'The access keys could not be loaded.')}
          </Alert>
        </Box>
      )}

      {pendingDelete !== null && (
        <DeleteAccessKeyModal
          accessKey={pendingDelete}
          onClose={() => {
            setPendingDelete(null)
            actions.setSelectedItems([])
          }}
        />
      )}
    </>
  )
}

/** DeleteAccessKeyModal confirms revoking a key. */
function DeleteAccessKeyModal({
  accessKey,
  onClose,
}: {
  accessKey: AccessKey
  onClose: () => void
}) {
  const dispatch = useAppDispatch()
  const [deleteAccessKey, { isLoading }] = useDeleteAccessKeyMutation()
  const [error, setError] = useState<ApiError>(undefined)

  async function confirm() {
    try {
      await deleteAccessKey(accessKey.access_key).unwrap()
      dispatch(
        notify({
          type: 'success',
          header: 'Access key deleted',
          content: `${accessKey.name} (${accessKey.access_key}) no longer works.`,
        }),
      )
      onClose()
    } catch (err) {
      setError(err as ApiError)
    }
  }

  return (
    <Modal
      visible
      onDismiss={onClose}
      header={`Delete ${accessKey.name}?`}
      closeAriaLabel="Close"
      footer={
        <Box float="right">
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" onClick={onClose}>
              Cancel
            </Button>
            <Button
              variant="primary"
              loading={isLoading}
              onClick={() => void confirm()}
              data-testid="confirm-delete-access-key"
            >
              Delete
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <Box variant="p">
          Applications using <Box variant="code">{accessKey.access_key}</Box> will be refused from
          their next request. This cannot be undone.
        </Box>
        {error !== undefined && (
          <Alert type="error" header="The access key was not deleted">
            {errorMessage(error, 'The access key could not be deleted.')}
          </Alert>
        )}
      </SpaceBetween>
    </Modal>
  )
}
