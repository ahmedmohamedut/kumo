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
  clustersApi,
  pollInterval,
  useListClustersQuery,
  type Cluster,
} from '@/features/kubernetes/api'
import { columnDefinitions, columnDisplay, columnOptions } from '@/features/kubernetes/columns'
import { DeleteClusterModal } from '@/features/kubernetes/DeleteClusterModal'
import {
  KubernetesGate,
  NoKubernetesImageAlert,
  useKubernetesVersions,
} from '@/features/kubernetes/KubernetesGate'
import { canDelete } from '@/features/kubernetes/state'
import { errorMessage } from '@/lib/apiError'

const pageSizes = [10, 20, 50]

const defaultPreferences: CollectionPreferencesProps.Preferences = {
  pageSize: 20,
  wrapLines: false,
  stripedRows: true,
  contentDisplay: columnDisplay,
}

/** ClustersListPage is the Kubernetes → Clusters page. */
export function ClustersListPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Kubernetes', href: '/kubernetes' },
      { text: 'Clusters', href: '/kubernetes' },
    ],
  })
  return (
    <KubernetesGate title="Clusters">
      <ClustersTable />
    </KubernetesGate>
  )
}

/** ClustersTable lists the clusters, polling while any is moving. */
function ClustersTable() {
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { versions } = useKubernetesVersions()
  const cached = useAppSelector(clustersApi.endpoints.listClusters.select())
  const { data, isLoading, isFetching, error, refetch } = useListClustersQuery(undefined, {
    pollingInterval: pollInterval(cached.data?.items),
  })
  const [preferences, setPreferences] = useState(defaultPreferences)
  const [pendingDelete, setPendingDelete] = useState<Cluster | null>(null)

  const clusters = data?.items ?? []
  const { items, actions, collectionProps, filterProps, filteredItemsCount, paginationProps } =
    useCollection(clusters, {
      filtering: { fields: ['name', 'endpoint', 'kubernetes_version'] },
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
            A cluster is a Kubernetes control plane and a group of workers Kumo runs for you,
            reachable from inside its VPC.
          </Box>
          {isAdmin && versions.length > 0 && (
            <Button onClick={() => navigate('/kubernetes/create')}>Create cluster</Button>
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
      {versions.length === 0 && <NoKubernetesImageAlert />}

      <Table
        {...collectionProps}
        items={items}
        loading={isLoading}
        loadingText="Loading clusters"
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
            description="Kubernetes clusters. Their nodes are virtual machines Kumo owns, reachable from inside their VPC."
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh clusters"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <Button
                  disabled={!isAdmin || selected === undefined || !canDelete(selected.state)}
                  onClick={() => selected !== undefined && setPendingDelete(selected)}
                  data-testid="delete-cluster"
                >
                  Delete
                </Button>
                <Button
                  variant="primary"
                  disabled={!isAdmin || versions.length === 0}
                  onClick={() => navigate('/kubernetes/create')}
                  data-testid="create-cluster"
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
        <DeleteClusterModal
          cluster={pendingDelete}
          onClose={() => setPendingDelete(null)}
          onDeleted={() => actions.setSelectedItems([])}
        />
      )}
    </>
  )
}
