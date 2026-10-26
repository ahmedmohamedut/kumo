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
import Link from '@cloudscape-design/components/link'
import Modal from '@cloudscape-design/components/modal'
import Pagination from '@cloudscape-design/components/pagination'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import Table from '@cloudscape-design/components/table'
import TextFilter from '@cloudscape-design/components/text-filter'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import {
  useDeleteInstanceTypeMutation,
  useGetInstanceTypeQuery,
  useListInstanceTypesQuery,
  type InstanceType,
} from '@/features/instancetypes/api'
import { columnDefinitions, columnDisplay, columnOptions } from '@/features/instancetypes/columns'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'

const pageSizes = [10, 20, 50]

const defaultPreferences: CollectionPreferencesProps.Preferences = {
  pageSize: 20,
  wrapLines: false,
  stripedRows: true,
  contentDisplay: columnDisplay,
}

/**
 * InstanceTypesListPage lists the sizes an instance can be launched at, and
 * is where an administrator creates, edits and deletes them.
 *
 * The write actions are disabled for a viewer rather than left to answer
 * `403`: the API is the guard, the console just does not offer a button that
 * can only fail.
 */
export function InstanceTypesListPage() {
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { data, isLoading, isFetching, error, refetch } = useListInstanceTypesQuery()
  const [preferences, setPreferences] = useState(defaultPreferences)
  const [pendingDelete, setPendingDelete] = useState<InstanceType | null>(null)

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Instance types', href: '/instance-types' },
    ],
  })

  const types = data ?? []

  const { items, actions, collectionProps, filterProps, filteredItemsCount, paginationProps } =
    useCollection(types, {
      filtering: { fields: ['id', 'description'] },
      // The API already returns the types smallest first, which is the useful
      // default; a column sort is the operator's choice on top of it.
      sorting: {},
      pagination: { pageSize: preferences.pageSize },
      selection: { trackBy: 'id' },
    })
  const selected = collectionProps.selectedItems?.[0]

  // Built after the collection hook so the Clear filter button can use its
  // actions; the table shows it whenever the current page has no rows.
  const empty =
    types.length === 0 ? (
      <Box textAlign="center" padding="l">
        <SpaceBetween size="s" alignItems="center">
          <Header variant="h2">No instance types</Header>
          <Box variant="p">An instance can only be launched at a size defined here.</Box>
          {isAdmin && (
            <Button onClick={() => navigate('/instance-types/create')}>Create instance type</Button>
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
      <Table
        {...collectionProps}
        items={items}
        loading={isLoading}
        loadingText="Loading instance types"
        empty={empty}
        columnDefinitions={columnDefinitions}
        columnDisplay={preferences.contentDisplay}
        wrapLines={preferences.wrapLines}
        stripedRows={preferences.stripedRows}
        selectionType={isAdmin ? 'single' : undefined}
        trackBy="id"
        variant="full-page"
        ariaLabels={{
          selectionGroupLabel: 'Instance type selection',
          itemSelectionLabel: (_data, item) => `Select ${item.id}`,
        }}
        header={
          <Header
            variant="awsui-h1-sticky"
            counter={types.length > 0 ? `(${types.length})` : undefined}
            description={
              isAdmin
                ? 'The sizes instances are launched at. A change applies to later launches; running instances keep their size.'
                : 'The sizes instances are launched at. Only an administrator can change them.'
            }
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh instance types"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <Button
                  disabled={!isAdmin || selected === undefined}
                  onClick={() =>
                    selected && navigate(`/instance-types/${encodeURIComponent(selected.id)}/edit`)
                  }
                >
                  Edit
                </Button>
                <Button
                  disabled={!isAdmin || selected === undefined}
                  onClick={() => selected && setPendingDelete(selected)}
                  data-testid="delete-instance-type"
                >
                  Delete
                </Button>
                <Button
                  variant="primary"
                  disabled={!isAdmin}
                  onClick={() => navigate('/instance-types/create')}
                >
                  Create instance type
                </Button>
              </SpaceBetween>
            }
          >
            Instance types
          </Header>
        }
        filter={
          <TextFilter
            {...filterProps}
            filteringAriaLabel="Filter instance types"
            filteringPlaceholder="Find instance types"
            countText={`${filteredItemsCount ?? 0} matches`}
          />
        }
        pagination={
          <Pagination
            {...paginationProps}
            ariaLabels={{ paginationLabel: 'Instance type pagination' }}
          />
        }
        preferences={
          <CollectionPreferences
            title="Preferences"
            confirmLabel="Confirm"
            cancelLabel="Cancel"
            preferences={preferences}
            onConfirm={({ detail }) => setPreferences(detail)}
            pageSizePreference={{
              title: 'Instance types per page',
              options: pageSizes.map((size) => ({ value: size, label: `${size} types` })),
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
          <Alert type="error" header="The instance types could not be loaded">
            {errorMessage(error, 'The instance types could not be loaded.')}
          </Alert>
        </Box>
      )}

      {pendingDelete !== null && (
        <DeleteInstanceTypeModal
          instanceType={pendingDelete}
          onClose={() => setPendingDelete(null)}
        />
      )}
    </>
  )
}

interface DeleteInstanceTypeModalProps {
  instanceType: InstanceType
  onClose: () => void
}

/**
 * DeleteInstanceTypeModal confirms a delete, and first asks the API which
 * instances use the type. The API refuses the delete while any do (terminated
 * rows included, because of the foreign key), so the dialog says so up front
 * and names them instead of offering a button that answers `409`.
 */
function DeleteInstanceTypeModal({ instanceType, onClose }: DeleteInstanceTypeModalProps) {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const {
    data: detail,
    isFetching,
    error,
  } = useGetInstanceTypeQuery(instanceType.id, {
    refetchOnMountOrArgChange: true,
  })
  const [deleteInstanceType, { isLoading: isDeleting, error: deleteError }] =
    useDeleteInstanceTypeMutation()

  const usedBy = detail?.used_by ?? []
  const inUse = usedBy.length > 0

  async function confirm() {
    try {
      await deleteInstanceType(instanceType.id).unwrap()
      dispatch(
        notify({
          type: 'success',
          header: 'Instance type deleted',
          content: `${instanceType.id} is no longer offered when launching an instance.`,
        }),
      )
      onClose()
    } catch {
      // The Alert in the dialog reports it.
    }
  }

  return (
    <Modal
      visible
      onDismiss={onClose}
      header={`Delete ${instanceType.id}?`}
      closeAriaLabel="Close"
      footer={
        <Box float="right">
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" onClick={onClose}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={isFetching || inUse || error !== undefined}
              loading={isDeleting}
              onClick={() => void confirm()}
              data-testid="confirm-delete"
            >
              Delete
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="m">
        {isFetching ? (
          <Spinner />
        ) : error !== undefined ? (
          <Alert type="error" header="The instance type could not be checked">
            {errorMessage(error, 'Whether any instance uses this type could not be checked.')}
          </Alert>
        ) : inUse ? (
          <Alert type="warning" header="This instance type is in use">
            <SpaceBetween size="xs">
              <span>It cannot be deleted while instances reference it. Terminate these first:</span>
              <ul aria-label="Instances using this type">
                {usedBy.map((id) => (
                  <li key={id}>
                    <Link
                      href={`/instances/${id}`}
                      onFollow={(event) => {
                        event.preventDefault()
                        navigate(`/instances/${id}`)
                      }}
                    >
                      {id}
                    </Link>
                  </li>
                ))}
              </ul>
            </SpaceBetween>
          </Alert>
        ) : (
          <Box variant="p">
            {instanceType.id} will no longer be offered when launching an instance. This cannot be
            undone.
          </Box>
        )}

        {deleteError !== undefined && (
          <Alert type="error" header="The instance type was not deleted">
            {errorMessage(deleteError as ApiError, 'The instance type could not be deleted.')}
          </Alert>
        )}
      </SpaceBetween>
    </Modal>
  )
}
