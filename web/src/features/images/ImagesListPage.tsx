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
import { useAppDispatch, useAppSelector } from '@/app/store'
import {
  columnDisplayPreferences,
  columnOptions,
  defaultColumns,
  type Image,
} from '@/features/images/columns'
import {
  imagesApi,
  pollInterval,
  useDeleteImageMutation,
  useListImagesQuery,
} from '@/features/images/api'
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
    { id: 'id', label: 'Image ID', visible: true },
    { id: 'os', label: 'OS', visible: true },
    { id: 'architecture', label: 'Architecture', visible: true },
    { id: 'state', label: 'State', visible: true },
    { id: 'size', label: 'Size', visible: true },
    { id: 'min_disk_gb', label: 'Min disk', visible: true },
    { id: 'created_at', label: 'Created', visible: true },
  ],
  wrapLinesEnabled: true,
  stripedRowsEnabled: true,
}

/**
 * ImagesListPage lists the images on this node.
 *
 * Filtering runs on the server (`?q=`) rather than in `useCollection`, so a search
 * covers every image instead of only the page that happens to be loaded; the hook
 * then handles sorting and paging over the page it got back.
 *
 * The poll interval follows the data: 5 s while an image is importing or being
 * deleted, 30 s once everything has settled (03-frontend.md §5). An operator who
 * leaves this page open during a 600 MB import should watch it arrive, and should
 * not pay for a 5 s loop over a settled table afterwards.
 */
export function ImagesListPage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()

  const query = searchParams.get('q') ?? ''
  // `args` and the selector built from it are memoized so the subscription key
  // stays stable across renders; a fresh object every render would re-subscribe
  // on each keystroke.
  const args = useMemo(() => ({ query }), [query])
  // The cached page is read with a selector because the poll interval has to be
  // chosen before the subscription starts, and the data cannot come from the very
  // call that sets it up. Reading the cache triggers no request.
  const cached = useAppSelector(imagesApi.endpoints.listImages.select(args))
  const { data, isLoading, isFetching, error, refetch } = useListImagesQuery(args, {
    pollingInterval: pollInterval(cached.data?.items),
  })

  const [deleteImage, { isLoading: isDeleting }] = useDeleteImageMutation()
  const [pendingDelete, setPendingDelete] = useState<ReadonlyArray<Image>>([])

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Images', href: '/images' },
    ],
  })

  const items = data?.items ?? []

  const {
    items: visibleItems,
    collectionProps,
    paginationProps,
  } = useCollection(items, {
    // The URL owns the filter: the box shows `?q=` so a deep link looks the same
    // as a typed search. `defaultFilteringText` seeds the hook with it, so the
    // page that came back is filtered by the same text before it is sorted and
    // paged — the client-side pass is a no-op on an already filtered page.
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
        // A delete is a task, so a 202 means "queued", not "gone". The image
        // stays in the table in `deleting` until the task finishes.
        await deleteImage(target.id).unwrap()
      } catch (err) {
        // An image an instance boots from refuses with a 409 naming the
        // instances. Report which image failed rather than one opaque failure.
        failures.push(
          `${target.name}: ${errorMessage(err as ApiError, 'the image could not be deleted.')}`,
        )
      }
    }

    if (failures.length === 0) {
      dispatch(
        notify({
          type: 'in-progress',
          header: targets.length === 1 ? 'Image deletion started' : 'Image deletions started',
          content: `${targets.length} image(s) are being deleted. The volume is removed by a background task.`,
        }),
      )
      return
    }

    dispatch(
      notify({
        type: failures.length === targets.length ? 'error' : 'warning',
        header: 'Some images were not deleted',
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
        loadingText="Loading images"
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
            description="Guest images instances on this node can be launched from."
            actions={
              <SpaceBetween direction="horizontal" size="s">
                <Button
                  disabled={selection.length === 0}
                  onClick={() => setPendingDelete(selection)}
                  data-testid="delete-images"
                >
                  Delete
                  {selection.length > 0 ? ` (${selection.length})` : ''}
                </Button>
                <Button
                  variant="primary"
                  onClick={() => navigate('/images/import')}
                  data-testid="import-image"
                >
                  Import image
                </Button>
              </SpaceBetween>
            }
          >
            Images
          </Header>
        }
        filter={
          <SpaceBetween size="s">
            <TextFilter
              filteringText={query}
              filteringAriaLabel="Filter images"
              filteringPlaceholder="Find images"
              countText={`${items.length} images`}
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
              ariaLabel="Refresh images"
              loading={isFetching}
              onClick={() => void refetch()}
            />
          </SpaceBetween>
        }
        pagination={
          <Pagination {...paginationProps} ariaLabels={{ paginationLabel: 'Image pagination' }} />
        }
        preferences={
          <CollectionPreferences
            title="Preferences"
            confirmLabel="Confirm"
            cancelLabel="Cancel"
            preferences={preferences}
            pageSizePreference={{
              title: 'Images per page',
              options: pageSizes.map((size) => ({ value: size, label: `${size} images` })),
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
                {query === '' ? 'No images' : `No images match "${query}"`}
              </Header>
              {query === '' ? (
                <>
                  <Box variant="p">
                    Import an image from a URL, or pick one of the well-known cloud images. Kumo
                    downloads it once and every instance launches from the local copy.
                  </Box>
                  <Button variant="primary" onClick={() => navigate('/images/import')}>
                    Import your first image
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
          <Alert type="error" header="The images could not be loaded">
            {errorMessage(error, 'The images could not be loaded.')}
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
              : `Delete ${pendingDelete.length} images?`
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
              Deleting an image removes the downloaded copy from this node. Instances that already
              launched keep their own root disk, but no new instance can launch from this image.
            </Alert>

            <Box variant="p">
              {pendingDelete.length === 1
                ? 'This image will no longer be offered when launching an instance.'
                : `${pendingDelete.length} images will no longer be offered when launching an instance.`}{' '}
              An image an instance still references cannot be deleted; terminate the instance first.
            </Box>

            {pendingDelete.some((image) => image.state === 'failed') && (
              <Alert type="info" header="Some of these never finished importing">
                An image whose import failed holds no volume, so deleting it only clears the failed
                record.
              </Alert>
            )}
          </SpaceBetween>
        </Modal>
      )}
    </>
  )
}
