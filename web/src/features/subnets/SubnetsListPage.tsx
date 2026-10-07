import { useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useCollection } from '@cloudscape-design/collection-hooks'
import type { SelectProps } from '@cloudscape-design/components'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import CollectionPreferences from '@cloudscape-design/components/collection-preferences'
import Header from '@cloudscape-design/components/header'
import Modal from '@cloudscape-design/components/modal'
import Pagination from '@cloudscape-design/components/pagination'
import Select from '@cloudscape-design/components/select'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Table from '@cloudscape-design/components/table'
import TextFilter from '@cloudscape-design/components/text-filter'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch } from '@/app/store'
import {
  columnDisplayPreferences,
  columnOptions,
  defaultColumns,
  type Subnet,
} from '@/features/subnets/columns'
import { useDeleteSubnetMutation, useListSubnetsQuery } from '@/features/subnets/api'
import { useListVPCsQuery } from '@/features/vpcs/api'
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

/** ALL_VPCS is the "every VPC" choice of the filter, distinct from no VPCs at all. */
const ALL_VPCS = '__all__'

/**
 * SubnetsListPage lists the subnets on this node.
 *
 * Two filters compose, and both live in the URL: a VPC picker (`?vpc_id=`) and
 * a name search (`?q=`) that runs on the server, so a search covers every
 * subnet of the picked VPC rather than only the loaded page.
 *
 * The connected-VPC column needs the VPC names, which come from the same
 * lookup the picker is built from — one extra list request for the whole page,
 * not one per row.
 */
export function SubnetsListPage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()

  const vpcId = searchParams.get('vpc_id') ?? ''
  const query = searchParams.get('q') ?? ''
  // `args` is memoized so the subscription key stays stable across renders.
  const args = useMemo(() => ({ vpcId, query }), [vpcId, query])
  const { data, isLoading, isFetching, error, refetch } = useListSubnetsQuery(args)
  const { data: vpcsData } = useListVPCsQuery()

  const [deleteSubnet, { isLoading: isDeleting }] = useDeleteSubnetMutation()
  const [pendingDelete, setPendingDelete] = useState<ReadonlyArray<Subnet>>([])

  usePageMeta({
    breadcrumbs: [
      { text: 'Networking', href: '/' },
      { text: 'Subnets', href: '/subnets' },
    ],
  })

  const vpcs = vpcsData?.items ?? []
  const vpcNames = new Map(vpcs.map((vpc) => [vpc.id, vpc.name]))
  const items = data?.items ?? []

  const allOptions: ReadonlyArray<SelectProps.Option> = [
    { value: ALL_VPCS, label: 'All VPCs' },
    ...vpcs.map((vpc) => ({
      value: vpc.id,
      label: vpc.name,
      description: vpc.cidr,
    })),
  ]

  const {
    items: visibleItems,
    collectionProps,
    paginationProps,
  } = useCollection(items, {
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
        // A subnet is a row, so the 204 means it is gone immediately; a subnet
        // an interface still holds an address from refuses with a 409 first.
        await deleteSubnet(target.id).unwrap()
      } catch (err) {
        failures.push(
          `${target.name}: ${errorMessage(err as ApiError, 'the subnet could not be deleted.')}`,
        )
      }
    }

    if (failures.length === 0) {
      dispatch(
        notify({
          type: 'success',
          header: targets.length === 1 ? 'Subnet deleted' : 'Subnets deleted',
          content: `${targets.length} subnet(s) deleted.`,
        }),
      )
      return
    }

    dispatch(
      notify({
        type: failures.length === targets.length ? 'error' : 'warning',
        header: 'Some subnets were not deleted',
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
        loadingText="Loading subnets"
        columnDefinitions={defaultColumns(vpcNames)}
        columnDisplay={columnDisplayPreferences}
        wrapLines={preferences.wrapLines}
        stripedRows={preferences.stripedRows}
        selectionType="multi"
        trackBy="id"
        variant="full-page"
        header={
          <Header
            counter={items.length > 0 ? `(${items.length})` : undefined}
            description="Address partitions of a VPC that instances attach to."
            actions={
              <SpaceBetween direction="horizontal" size="s">
                <Button
                  disabled={selection.length === 0}
                  onClick={() => setPendingDelete(selection)}
                  data-testid="delete-subnets"
                >
                  Delete
                  {selection.length > 0 ? ` (${selection.length})` : ''}
                </Button>
                <Button
                  variant="primary"
                  onClick={() => navigate('/subnets/create')}
                  data-testid="create-subnet"
                >
                  Create subnet
                </Button>
              </SpaceBetween>
            }
          >
            Subnets
          </Header>
        }
        filter={
          <SpaceBetween size="s">
            {/* The VPC filter lives in the URL, so a subnet search is linkable
                and a reload does not silently widen it to every VPC. */}
            <Select
              selectedOption={
                allOptions.find((option) => option.value === (vpcId === '' ? ALL_VPCS : vpcId)) ??
                allOptions[0] ??
                null
              }
              onChange={({ detail }) =>
                setSearchParams(
                  detail.selectedOption?.value === undefined ||
                    detail.selectedOption.value === ALL_VPCS
                    ? {}
                    : { vpc_id: detail.selectedOption.value },
                  { replace: true },
                )
              }
              options={allOptions}
              ariaLabel="Filter by VPC"
            />
            <TextFilter
              filteringText={query}
              filteringAriaLabel="Filter subnets"
              filteringPlaceholder="Find subnets"
              countText={`${items.length} subnets`}
              onChange={({ detail }) =>
                setSearchParams(
                  detail.filteringText === ''
                    ? vpcId === ''
                      ? {}
                      : { vpc_id: vpcId }
                    : vpcId === ''
                      ? { q: detail.filteringText }
                      : { vpc_id: vpcId, q: detail.filteringText },
                  { replace: true },
                )
              }
            />
            <Button
              iconName="refresh"
              ariaLabel="Refresh subnets"
              loading={isFetching}
              onClick={() => void refetch()}
            />
          </SpaceBetween>
        }
        pagination={
          <Pagination {...paginationProps} ariaLabels={{ paginationLabel: 'Subnet pagination' }} />
        }
        preferences={
          <CollectionPreferences
            title="Preferences"
            confirmLabel="Confirm"
            cancelLabel="Cancel"
            preferences={preferences}
            pageSizePreference={{
              title: 'Subnets per page',
              options: pageSizes.map((size) => ({ value: size, label: `${size} subnets` })),
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
              <Header variant="h2">
                {query === ''
                  ? 'No subnets'
                  : vpcId === ''
                    ? `No subnets match "${query}"`
                    : `No subnets in this VPC match "${query}"`}
              </Header>
              {query === '' && vpcId === '' ? (
                <>
                  <Box variant="p">
                    The default VPC carries a default subnet at startup. Add another one to split an
                    address block between groups of instances.
                  </Box>
                  <Button variant="primary" onClick={() => navigate('/subnets/create')}>
                    Create your first subnet
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
          <Alert type="error" header="The subnets could not be loaded">
            {errorMessage(error, 'The subnets could not be loaded.')}
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
              : `Delete ${pendingDelete.length} subnets?`
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
                  data-testid="confirm-delete-subnets"
                >
                  Delete
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Alert type="warning" header="This cannot be undone">
              Deleting a subnet frees its address block back to the VPC.
            </Alert>

            <Box variant="p">
              {pendingDelete.length === 1
                ? 'A subnet an instance still holds an address from cannot be deleted; terminate the instances first.'
                : `${pendingDelete.length} subnets will be deleted; any one still holding an address will be refused.`}
            </Box>
          </SpaceBetween>
        </Modal>
      )}
    </>
  )
}
