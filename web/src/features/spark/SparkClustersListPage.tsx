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
import {
  sparkApi,
  pollInterval,
  useListSparkClustersQuery,
  type SparkCluster,
} from '@/features/spark/api'
import { columnDefinitions, columnDisplay, columnOptions } from '@/features/spark/columns'
import { DeleteSparkClusterModal } from '@/features/spark/DeleteSparkClusterModal'
import { SparkGate, NoSparkImageAlert, useSpark } from '@/features/spark/SparkGate'
import { canDelete } from '@/features/spark/state'
import { errorMessage } from '@/lib/apiError'

const pageSizes = [10, 20, 50]

const defaultPreferences: CollectionPreferencesProps.Preferences = {
  pageSize: 20,
  wrapLines: false,
  stripedRows: true,
  contentDisplay: columnDisplay,
}

/** SparkClustersListPage is the Spark → Clusters page. */
export function SparkClustersListPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Spark', href: '/spark' },
      { text: 'Clusters', href: '/spark' },
    ],
  })
  return (
    <SparkGate title="Clusters">
      <SparkClustersTable />
    </SparkGate>
  )
}

/** SparkClustersTable lists the clusters, polling while any is moving. */
function SparkClustersTable() {
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { versions } = useSpark()
  const cached = useAppSelector(sparkApi.endpoints.listSparkClusters.select())
  const { data, isLoading, isFetching, error, refetch } = useListSparkClustersQuery(undefined, {
    pollingInterval: pollInterval(cached.data?.items),
  })
  const [preferences, setPreferences] = useState(defaultPreferences)
  const [pendingDelete, setPendingDelete] = useState<SparkCluster | null>(null)

  const clusters = data?.items ?? []
  const { items, actions, collectionProps, filterProps, filteredItemsCount, paginationProps } =
    useCollection(clusters, {
      filtering: { fields: ['name', 'endpoint', 'spark_version'] },
      sorting: {},
      pagination: { pageSize: preferences.pageSize },
      selection: { trackBy: 'id' },
    })
  const selected = collectionProps.selectedItems?.[0]

  const empty =
    clusters.length === 0 ? (
      <Box textAlign="center" padding="l">
        <SpaceBetween size="s" alignItems="center">
          <Header variant="h2">No clusters</Header>
          <Box variant="p">
            A Spark cluster is a master and a group of workers Kumo runs for you, reachable from
            inside its VPC.
          </Box>
          {isAdmin && versions.length > 0 && (
            <Button onClick={() => navigate('/spark/create')}>Create cluster</Button>
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

  return (
    <>
      {versions.length === 0 && <NoSparkImageAlert />}

      <Table
        {...collectionProps}
        items={items}
        loading={isLoading}
        loadingText="Loading Spark clusters"
        empty={empty}
        columnDefinitions={columnDefinitions}
        columnDisplay={preferences.contentDisplay}
        wrapLines={preferences.wrapLines}
        stripedRows={preferences.stripedRows}
        selectionType={isAdmin ? 'single' : undefined}
        isItemDisabled={(item) => item.state === 'deleting'}
        trackBy="id"
        variant="full-page"
        ariaLabels={{
          selectionGroupLabel: 'Cluster selection',
          itemSelectionLabel: (_data, item) => `Select ${item.name}`,
        }}
        header={
          <Header
            variant="awsui-h1-sticky"
            counter={clusters.length > 0 ? `(${clusters.length})` : undefined}
            description="Spark clusters. Their nodes are virtual machines Kumo owns, reachable from inside their VPC."
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh Spark clusters"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <Button
                  disabled={!isAdmin || selected === undefined || !canDelete(selected.state)}
                  onClick={() => selected !== undefined && setPendingDelete(selected)}
                  data-testid="delete-spark-cluster"
                >
                  Delete
                </Button>
                <Button
                  variant="primary"
                  disabled={!isAdmin || versions.length === 0}
                  onClick={() => navigate('/spark/create')}
                  data-testid="create-spark-cluster"
                >
                  Create cluster
                </Button>
              </SpaceBetween>
            }
          >
            Clusters
          </Header>
        }
        filter={
          <TextFilter
            {...filterProps}
            filteringAriaLabel="Filter clusters"
            filteringPlaceholder="Find clusters"
            countText={`${filteredItemsCount ?? 0} matches`}
          />
        }
        pagination={
          <Pagination {...paginationProps} ariaLabels={{ paginationLabel: 'Cluster pagination' }} />
        }
        preferences={
          <CollectionPreferences
            title="Preferences"
            confirmLabel="Confirm"
            cancelLabel="Cancel"
            preferences={preferences}
            onConfirm={({ detail }) => setPreferences(detail)}
            pageSizePreference={{
              title: 'Clusters per page',
              options: pageSizes.map((size) => ({ value: size, label: `${size} clusters` })),
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
          <Alert type="error" header="The clusters could not be loaded">
            {errorMessage(error, 'The clusters could not be loaded.')}
          </Alert>
        </Box>
      )}

      {pendingDelete !== null && (
        <DeleteSparkClusterModal
          cluster={pendingDelete}
          onClose={() => setPendingDelete(null)}
          onDeleted={() => actions.setSelectedItems([])}
        />
      )}
    </>
  )
}
