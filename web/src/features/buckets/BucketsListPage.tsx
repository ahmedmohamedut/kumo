import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useCollection } from '@cloudscape-design/collection-hooks'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import CollectionPreferences, {
  type CollectionPreferencesProps,
} from '@cloudscape-design/components/collection-preferences'
import Header from '@cloudscape-design/components/header'
import Pagination from '@cloudscape-design/components/pagination'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Table from '@cloudscape-design/components/table'
import TextFilter from '@cloudscape-design/components/text-filter'

import { usePageMeta } from '@/app/pageMeta'
import { useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import { bucketsApi, pollInterval, useListBucketsQuery } from '@/features/buckets/api'
import { columnDefinitions, columnDisplay, columnOptions } from '@/features/buckets/columns'
import { DeleteBucketModal } from '@/features/buckets/DeleteBucketModal'
import { ObjectStorageGate } from '@/features/buckets/ObjectStorageGate'
import { errorMessage } from '@/lib/apiError'

const pageSizes = [10, 20, 50]

const defaultPreferences: CollectionPreferencesProps.Preferences = {
  pageSize: 20,
  wrapLines: false,
  stripedRows: true,
  contentDisplay: columnDisplay,
}

/** BucketsListPage is the Storage → Buckets page, behind the object storage gate. */
export function BucketsListPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Storage', href: '/buckets' },
      { text: 'Buckets', href: '/buckets' },
    ],
  })

  return (
    <ObjectStorageGate title="Buckets">
      <BucketsTable />
    </ObjectStorageGate>
  )
}

/**
 * BucketsTable lists every bucket on the object storage server, including the
 * ones made with another client, which show no creator.
 *
 * The list polls only while a forced delete is emptying a bucket: it is read
 * from the object store on every request, and a bucket has no other state that
 * changes on its own.
 */
function BucketsTable() {
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const cached = useAppSelector(bucketsApi.endpoints.listBuckets.select())
  const { data, isLoading, isFetching, error, refetch } = useListBucketsQuery(undefined, {
    pollingInterval: pollInterval(cached.data),
  })
  const [preferences, setPreferences] = useState(defaultPreferences)
  const [pendingDelete, setPendingDelete] = useState<string | null>(null)

  const buckets = data ?? []

  const { items, actions, collectionProps, filterProps, filteredItemsCount, paginationProps } =
    useCollection(buckets, {
      filtering: { fields: ['name', 'created_by'] },
      sorting: {},
      pagination: { pageSize: preferences.pageSize },
      selection: { trackBy: 'name' },
    })
  const selected = collectionProps.selectedItems?.[0]

  const empty =
    buckets.length === 0 ? (
      <Box textAlign="center" padding="l">
        <SpaceBetween size="s" alignItems="center">
          <Header variant="h2">No buckets</Header>
          <Box variant="p">A bucket holds objects that applications read and write over S3.</Box>
          {isAdmin && <Button onClick={() => navigate('/buckets/create')}>Create bucket</Button>}
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

  return (
    <>
      <Table
        {...collectionProps}
        items={items}
        loading={isLoading}
        loadingText="Loading buckets"
        empty={empty}
        columnDefinitions={columnDefinitions}
        columnDisplay={preferences.contentDisplay}
        wrapLines={preferences.wrapLines}
        stripedRows={preferences.stripedRows}
        selectionType={isAdmin ? 'single' : undefined}
        isItemDisabled={(item) => item.state === 'deleting'}
        trackBy="name"
        variant="full-page"
        ariaLabels={{
          selectionGroupLabel: 'Bucket selection',
          itemSelectionLabel: (_data, item) => `Select ${item.name}`,
        }}
        header={
          <Header
            variant="awsui-h1-sticky"
            counter={buckets.length > 0 ? `(${buckets.length})` : undefined}
            description="S3-compatible buckets on this node's object storage server."
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh buckets"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <Button
                  disabled={!isAdmin || selected === undefined}
                  onClick={() => selected && setPendingDelete(selected.name)}
                  data-testid="delete-bucket"
                >
                  Delete
                </Button>
                <Button
                  variant="primary"
                  disabled={!isAdmin}
                  onClick={() => navigate('/buckets/create')}
                >
                  Create bucket
                </Button>
              </SpaceBetween>
            }
          >
            Buckets
          </Header>
        }
        filter={
          <TextFilter
            {...filterProps}
            filteringAriaLabel="Filter buckets"
            filteringPlaceholder="Find buckets"
            countText={`${filteredItemsCount ?? 0} matches`}
          />
        }
        pagination={
          <Pagination {...paginationProps} ariaLabels={{ paginationLabel: 'Bucket pagination' }} />
        }
        preferences={
          <CollectionPreferences
            title="Preferences"
            confirmLabel="Confirm"
            cancelLabel="Cancel"
            preferences={preferences}
            onConfirm={({ detail }) => setPreferences(detail)}
            pageSizePreference={{
              title: 'Buckets per page',
              options: pageSizes.map((size) => ({ value: size, label: `${size} buckets` })),
            }}
            wrapLinesPreference={{
              label: 'Wrap lines',
              description: 'Show all text in each cell.',
            }}
            stripedRowsPreference={{ label: 'Striped rows', description: 'Alternate row shading.' }}
            contentDisplayPreference={{ title: 'Columns', options: columnOptions }}
          />
        }
      />

      {error !== undefined && (
        <Box padding="m">
          <Alert type="error" header="The buckets could not be loaded">
            {errorMessage(error, 'The buckets could not be loaded.')}
          </Alert>
        </Box>
      )}

      {pendingDelete !== null && (
        <DeleteBucketModal
          name={pendingDelete}
          onClose={() => setPendingDelete(null)}
          onDeleted={() => actions.setSelectedItems([])}
        />
      )}
    </>
  )
}
