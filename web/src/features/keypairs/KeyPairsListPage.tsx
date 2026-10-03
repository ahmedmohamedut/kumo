import { useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useCollection } from '@cloudscape-design/collection-hooks'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import ButtonDropdown from '@cloudscape-design/components/button-dropdown'
import Checkbox from '@cloudscape-design/components/checkbox'
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
  type KeyPair,
} from '@/features/keypairs/columns'
import { useDeleteKeyPairMutation, useListKeyPairsQuery } from '@/features/keypairs/api'
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
  contentDisplay: [
    { id: 'name', label: 'Name', visible: true },
    { id: 'id', label: 'Key pair ID', visible: true },
    { id: 'type', label: 'Type', visible: true },
    { id: 'fingerprint', label: 'Fingerprint', visible: true },
    { id: 'created_at', label: 'Created', visible: true },
  ],
  wrapLinesEnabled: true,
  stripedRowsEnabled: true,
}

/**
 * KeyPairsListPage lists the key pairs on this node.
 *
 * Filtering runs on the server (`?q=`) rather than in `useCollection`, so a search
 * covers every key pair instead of only the page that happens to be loaded;
 * `useCollection` then handles sorting and paging over the page it got back. The
 * `?q=` link on the name column and the deep link from the nav both land here
 * with that filter already applied.
 */
export function KeyPairsListPage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()

  const query = searchParams.get('q') ?? ''
  const { data, isLoading, isFetching, error, refetch } = useListKeyPairsQuery({ query })

  const [deleteKeyPair, { isLoading: isDeleting }] = useDeleteKeyPairMutation()
  const [pendingDelete, setPendingDelete] = useState<ReadonlyArray<KeyPair>>([])
  const [forceDelete, setForceDelete] = useState(false)

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Key pairs', href: '/key-pairs' },
    ],
  })

  const items = data?.items ?? []

  const {
    items: visibleItems,
    collectionProps,
    filterProps,
    paginationProps,
  } = useCollection(items, {
    filtering: { fields: ['name', 'id', 'fingerprint'] },
    pagination: { pageSize: preferences.pageSize },
    selection: {},
  })

  const { selectedItems } = collectionProps
  const selection = selectedItems ?? []

  async function confirmDelete() {
    const targets = pendingDelete
    const force = forceDelete
    setPendingDelete([])
    setForceDelete(false)

    const failures: string[] = []
    for (const target of targets) {
      try {
        await deleteKeyPair({ id: target.id, force }).unwrap()
      } catch (err) {
        // A key attached to a running instance refuses without force. Report
        // which one so the operator knows what to act on rather than seeing a
        // single opaque failure for a multi-key delete.
        failures.push(
          `${target.name}: ${errorMessage(err as ApiError, 'the key pair could not be deleted.')}`,
        )
      }
    }

    if (failures.length === 0) {
      dispatch(
        notify({
          type: 'success',
          header: targets.length === 1 ? 'Key pair deleted' : 'Key pairs deleted',
          content: `${targets.length} key pair(s) deleted. Running instances keep the keys already injected into them.`,
        }),
      )
      return
    }

    dispatch(
      notify({
        type: failures.length === targets.length ? 'error' : 'warning',
        header: 'Some key pairs were not deleted',
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
        loadingText="Loading key pairs"
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
            description="SSH key pairs instances on this node can be launched with."
            actions={
              <SpaceBetween direction="horizontal" size="s">
                <ButtonDropdown
                  onItemClick={({ detail }) => {
                    if (detail.id === 'create') {
                      navigate('/key-pairs/create')
                    } else {
                      navigate('/key-pairs/import')
                    }
                  }}
                  items={[
                    { id: 'create', text: 'Create key pair' },
                    { id: 'import', text: 'Import key pair' },
                  ]}
                >
                  Add key pair
                </ButtonDropdown>
                <Button
                  disabled={selection.length === 0}
                  onClick={() => setPendingDelete(selection)}
                  data-testid="delete-key-pairs"
                >
                  Delete
                  {selection.length > 0 ? ` (${selection.length})` : ''}
                </Button>
              </SpaceBetween>
            }
          >
            Key pairs
          </Header>
        }
        filter={
          <SpaceBetween size="s">
            <TextFilter
              {...filterProps}
              filteringAriaLabel="Filter key pairs"
              filteringPlaceholder="Find key pairs"
              countText={`${items.length} key pairs`}
              onChange={({ detail }) => {
                // Cloudscape's ChangeDetail and the collection hook's event shape
                // are the same object, so hand the hook the wrapper it expects.
                filterProps.onChange({ detail })
                const next = detail.filteringText
                // Keep the filter in the URL so the list is linkable and the
                // reload does not silently widen the search.
                setSearchParams(next === '' ? {} : { q: next }, { replace: true })
              }}
            />
            <Button
              iconName="refresh"
              ariaLabel="Refresh key pairs"
              loading={isFetching}
              onClick={() => void refetch()}
            />
          </SpaceBetween>
        }
        pagination={
          <Pagination
            {...paginationProps}
            ariaLabels={{ paginationLabel: 'Key pair pagination' }}
          />
        }
        preferences={
          <CollectionPreferences
            title="Preferences"
            confirmLabel="Confirm"
            cancelLabel="Cancel"
            preferences={preferences}
            pageSizePreference={{
              title: 'Key pairs per page',
              options: pageSizes.map((size) => ({
                value: size,
                label: `${size} key pairs`,
              })),
            }}
            wrapLinesPreference={{
              label: 'Wrap lines',
              description: 'Show all text in each cell.',
            }}
            stripedRowsPreference={{ label: 'Striped rows', description: 'Alternate row shading.' }}
            contentDisplayPreference={{
              title: 'Columns',
              options: columnOptions,
            }}
          />
        }
        empty={
          <Box textAlign="center" padding="l">
            <SpaceBetween size="s" alignItems="center">
              <Header variant="h2">
                {query === '' ? 'No key pairs' : `No key pairs match "${query}"`}
              </Header>
              {query === '' ? (
                <>
                  <Box variant="p">
                    Create a key pair and Kumo generates it for you, or import a public key you
                    already have.
                  </Box>
                  <SpaceBetween direction="horizontal" size="s">
                    <Button variant="primary" onClick={() => navigate('/key-pairs/create')}>
                      Create key pair
                    </Button>
                    <Button onClick={() => navigate('/key-pairs/import')}>Import key pair</Button>
                  </SpaceBetween>
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
          <Alert type="error" header="The key pairs could not be loaded">
            {errorMessage(error, 'The key pairs could not be loaded.')}
          </Alert>
        </Box>
      )}

      {pendingDelete.length > 0 && (
        <Modal
          visible
          onDismiss={() => {
            setPendingDelete([])
            setForceDelete(false)
          }}
          header={
            pendingDelete.length === 1
              ? `Delete ${pendingDelete[0]?.name}?`
              : `Delete ${pendingDelete.length} key pairs?`
          }
          closeAriaLabel="Close"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="s">
                <Button
                  variant="link"
                  onClick={() => {
                    setPendingDelete([])
                    setForceDelete(false)
                  }}
                >
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  loading={isDeleting}
                  onClick={() => void confirmDelete()}
                  data-testid="confirm-delete"
                >
                  Delete
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Alert type="warning" header="This cannot be undone">
              Deleting a key pair removes the public key from this node. It does not remove the keys
              already injected into running instances, so they keep working; only new launches are
              affected.
            </Alert>

            <Box variant="p">
              {pendingDelete.length === 1
                ? 'This key pair will no longer be offered when launching an instance.'
                : `${pendingDelete.length} key pairs will no longer be offered when launching an instance.`}
            </Box>

            <Checkbox
              checked={forceDelete}
              onChange={({ detail }) => setForceDelete(detail.checked)}
              data-testid="force-delete"
            >
              Delete even if an instance is using the key pair
            </Checkbox>
          </SpaceBetween>
        </Modal>
      )}
    </>
  )
}
