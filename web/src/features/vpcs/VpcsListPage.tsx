import { useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useCollection } from '@cloudscape-design/collection-hooks'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import CollectionPreferences from '@cloudscape-design/components/collection-preferences'
import Header from '@cloudscape-design/components/header'
import Modal from '@cloudscape-design/components/modal'
import Pagination from '@cloudscape-design/components/pagination'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Table from '@cloudscape-design/components/table'
import TextFilter from '@cloudscape-design/components/text-filter'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch } from '@/app/store'
import {
  columnDisplayPreferences,
  columnOptions,
  defaultColumns,
  type VPC,
} from '@/features/vpcs/columns'
import { useDeleteVPCMutation, useListVPCsQuery } from '@/features/vpcs/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'

/** pageSizes are the table page sizes offered in the preferences panel. */
const pageSizes = [10, 20, 50, 100]

/** preferences are the table's page size and column visibility. */
const preferences = {
  pageSize: 20,
  pageSizes,
  wrapLines: false,
  stripedRows: true,
  contentDisplay: columnDisplayPreferences,
  wrapLinesEnabled: true,
  stripedRowsEnabled: true,
}

/**
 * VpcsListPage lists the VPCs on this node.
 *
 * Filtering runs on the server (`?q=`) rather than in `useCollection`, so a
 * search covers every VPC instead of only the page that happens to be loaded;
 * the hook then handles sorting and paging over the page it got back.
 *
 * A VPC is a private address block plus the libvirt network that carries it,
 * so deleting one is a task: the API answers `202` and the network is torn down
 * in the background. A VPC that still holds subnets — or instances inside them —
 * refuses with a `409` naming what is left, which is the guard this page shows
 * rather than guessing.
 */
export function VpcsListPage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()

  const query = searchParams.get('q') ?? ''
  // `args` is memoized so the subscription key stays stable across renders; a
  // fresh object every render would re-subscribe on each keystroke.
  const args = useMemo(() => ({ query }), [query])
  const { data, isLoading, isFetching, error, refetch } = useListVPCsQuery(args)

  const [deleteVPC, { isLoading: isDeleting }] = useDeleteVPCMutation()
  const [pendingDelete, setPendingDelete] = useState<ReadonlyArray<VPC>>([])

  usePageMeta({
    breadcrumbs: [
      { text: 'Networking', href: '/' },
      { text: 'VPCs', href: '/vpcs' },
    ],
  })

  const items = data?.items ?? []

  const {
    items: visibleItems,
    collectionProps,
    paginationProps,
  } = useCollection(items, {
    // The URL owns the filter: the box shows `?q=` so a deep link looks the same
    // as a typed search.
    filtering: { defaultFilteringText: query, fields: ['name'] },
    pagination: { pageSize: preferences.pageSize },
    selection: {},
  })

  const selection = collectionProps.selectedItems ?? []

  async function confirmDelete() {
    const targets = pendingDelete
    setPendingDelete([])

    const failures: string[] = []
    for (const target of targets) {
      try {
        // A delete is a task, so a 202 means "queued" — the network is torn down
        // in the background.
        await deleteVPC(target.id).unwrap()
      } catch (err) {
        // A VPC that still holds subnets (or instances) refuses with a 409
        // naming them. Report which VPC failed rather than one opaque failure.
        failures.push(
          `${target.name}: ${errorMessage(err as ApiError, 'the VPC could not be deleted.')}`,
        )
      }
    }

    if (failures.length === 0) {
      dispatch(
        notify({
          type: 'in-progress',
          header: targets.length === 1 ? 'VPC deletion started' : 'VPC deletions started',
          content: `${targets.length} VPC(s) are being deleted. The network is removed by a background task.`,
        }),
      )
      return
    }

    dispatch(
      notify({
        type: failures.length === targets.length ? 'error' : 'warning',
        header: 'Some VPCs were not deleted',
        content: failures.join('\n'),
      }),
    )
  }

  return (
    <>
      <Table
        {...collectionProps}
        items={visibleItems}
        loading={isLoading}
        loadingText="Loading VPCs"
        columnDefinitions={defaultColumns}
        columnDisplay={columnDisplayPreferences}
        wrapLines={preferences.wrapLines}
        stripedRows={preferences.stripedRows}
        selectionType="multi"
        trackBy="id"
        variant="full-page"
        header={
          <Header
            counter={items.length > 0 ? `(${items.length})` : undefined}
            description="Private address blocks instances can live in, one libvirt network each."
            actions={
              <SpaceBetween direction="horizontal" size="s">
                <Button
                  disabled={selection.length === 0}
                  onClick={() => setPendingDelete(selection)}
                  data-testid="delete-vpcs"
                >
                  Delete
                  {selection.length > 0 ? ` (${selection.length})` : ''}
                </Button>
                <Button
                  variant="primary"
                  onClick={() => navigate('/vpcs/create')}
                  data-testid="create-vpc"
                >
                  Create VPC
                </Button>
              </SpaceBetween>
            }
          >
            VPCs
          </Header>
        }
        filter={
          <SpaceBetween size="s">
            <TextFilter
              filteringText={query}
              filteringAriaLabel="Filter VPCs"
              filteringPlaceholder="Find VPCs"
              countText={`${items.length} VPCs`}
              // The filter lives in the URL so the list is linkable and a reload
              // does not silently widen the search.
              onChange={({ detail }) =>
                setSearchParams(detail.filteringText === '' ? {} : { q: detail.filteringText }, {
                  replace: true,
                })
              }
            />
            <Button
              iconName="refresh"
              ariaLabel="Refresh VPCs"
              loading={isFetching}
              onClick={() => void refetch()}
            />
          </SpaceBetween>
        }
        pagination={
          <Pagination {...paginationProps} ariaLabels={{ paginationLabel: 'VPC pagination' }} />
        }
        preferences={
          <CollectionPreferences
            title="Preferences"
            confirmLabel="Confirm"
            cancelLabel="Cancel"
            preferences={preferences}
            pageSizePreference={{
              title: 'VPCs per page',
              options: pageSizes.map((size) => ({ value: size, label: `${size} VPCs` })),
            }}
            wrapLinesPreference={{
              label: 'Wrap lines',
              description: 'Show all text in each cell.',
            }}
            stripedRowsPreference={{ label: 'Striped rows', description: 'Alternate row shading.' }}
            contentDisplayPreference={{ title: 'Columns', options: columnOptions }}
          />
        }
        empty={
          <Box textAlign="center" padding="l">
            <SpaceBetween size="s" alignItems="center">
              <Header variant="h2">{query === '' ? 'No VPCs' : `No VPCs match "${query}"`}</Header>
              {query === '' ? (
                <>
                  <Box variant="p">
                    The default VPC is created when the node starts. Add another one to give a group
                    of instances their own address block and bridge.
                  </Box>
                  <Button variant="primary" onClick={() => navigate('/vpcs/create')}>
                    Create your first VPC
                  </Button>
                </>
              ) : (
                <Button onClick={() => setSearchParams({}, { replace: true })}>Clear filter</Button>
              )}
            </SpaceBetween>
          </Box>
        }
      />

      {error !== undefined && (
        <Box padding="m">
          <Alert type="error" header="The VPCs could not be loaded">
            {errorMessage(error, 'The VPCs could not be loaded.')}
          </Alert>
        </Box>
      )}

      {pendingDelete.length > 0 && (
        <Modal
          visible
          onDismiss={() => setPendingDelete([])}
          header={
            pendingDelete.length === 1
              ? `Delete ${pendingDelete[0]?.name}?`
              : `Delete ${pendingDelete.length} VPCs?`
          }
          closeAriaLabel="Close"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="s">
                <Button variant="link" onClick={() => setPendingDelete([])}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  loading={isDeleting}
                  onClick={() => void confirmDelete()}
                  data-testid="confirm-delete-vpcs"
                >
                  Delete
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Alert type="warning" header="This cannot be undone">
              Deleting a VPC destroys its libvirt network and bridge, then removes the row.
            </Alert>

            <Box variant="p">
              {pendingDelete.length === 1
                ? 'A VPC that still contains subnets — or instances inside them — cannot be deleted; delete the subnets and terminate the instances first.'
                : `${pendingDelete.length} VPCs will be deleted; any one that still contains subnets or instances will be refused.`}
            </Box>
          </SpaceBetween>
        </Modal>
      )}
    </>
  )
}
