import { useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import BreadcrumbGroup from '@cloudscape-design/components/breadcrumb-group'
import Button from '@cloudscape-design/components/button'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import Modal from '@cloudscape-design/components/modal'
import Pagination from '@cloudscape-design/components/pagination'
import SpaceBetween from '@cloudscape-design/components/space-between'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import Table, { type TableProps } from '@cloudscape-design/components/table'

import { useAppDispatch } from '@/app/store'
import {
  bucketsApi,
  useCreateFolderMutation,
  useDeleteObjectsMutation,
  useListObjectsQuery,
  usePresignDownloadMutation,
  usePresignUploadMutation,
} from '@/features/buckets/api'
import { isPlainHTTP, putToPresignedURL, startDownload } from '@/features/buckets/transfer'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatBytes, formatRelativeTime } from '@/lib/format'

/** Entry is one row of the browser: a folder or an object. */
type Entry =
  | { kind: 'folder'; key: string }
  | { kind: 'object'; key: string; size: number; lastModified: string }

/** Upload is the progress of one file the operator picked. */
interface Upload {
  id: number
  name: string
  status: 'uploading' | 'done' | 'error'
  error?: string
}

interface ObjectBrowserProps {
  bucket: string
  /** canWrite is false for a viewer and while the bucket is being deleted. */
  canWrite: boolean
}

/**
 * ObjectBrowser is the Objects tab of a bucket: one folder at a time, like
 * the S3 console. The folder is in the URL (`?prefix=`), so a link to it and
 * the back button both work.
 *
 * Bytes never pass through kumod: an upload asks for a presigned PUT and
 * sends the file to the object store itself; a download asks for a presigned
 * GET and lets the browser save it (06-object-storage.md §1).
 */
export function ObjectBrowser({ bucket, canWrite }: ObjectBrowserProps) {
  const dispatch = useAppDispatch()
  const [searchParams, setSearchParams] = useSearchParams()
  const prefix = searchParams.get('prefix') ?? ''

  // cursors[i] is the cursor of page i+1; page 1 has none.
  const [cursors, setCursors] = useState<string[]>([])
  const [page, setPage] = useState(1)
  const cursor = page === 1 ? undefined : cursors[page - 2]

  const { data, isFetching, error, refetch } = useListObjectsQuery({
    name: bucket,
    prefix,
    ...(cursor === undefined ? {} : { cursor }),
  })
  const [presignUpload] = usePresignUploadMutation()
  const [presignDownload] = usePresignDownloadMutation()
  const [deleteObjects, { isLoading: isDeleting }] = useDeleteObjectsMutation()

  const [selected, setSelected] = useState<Entry[]>([])
  const [uploads, setUploads] = useState<Upload[]>([])
  const [isFolderOpen, setFolderOpen] = useState(false)
  const [isDeleteOpen, setDeleteOpen] = useState(false)
  const [actionError, setActionError] = useState<ApiError>(undefined)
  const fileInput = useRef<HTMLInputElement>(null)
  const uploadCounter = useRef(0)

  const entries: Entry[] = [
    ...(data?.folders ?? []).map((key): Entry => ({ kind: 'folder', key })),
    ...(data?.objects ?? []).map((object): Entry => ({
      kind: 'object',
      key: object.key,
      size: object.size_bytes,
      lastModified: object.last_modified,
    })),
  ]
  const nextCursor = data?.next_cursor ?? null

  function open(nextPrefix: string) {
    setSelected([])
    setCursors([])
    setPage(1)
    setSearchParams(nextPrefix === '' ? {} : { prefix: nextPrefix })
  }

  function goToPage(next: number) {
    if (next === page + 1 && nextCursor !== null) {
      setCursors((current) => [...current.slice(0, page - 1), nextCursor])
    }
    setSelected([])
    setPage(next)
  }

  async function upload(files: FileList) {
    const picked = Array.from(files)
    for (const file of picked) {
      const id = (uploadCounter.current += 1)
      setUploads((current) => [{ id, name: file.name, status: 'uploading' }, ...current])
      const settle = (status: Upload['status'], message?: string) =>
        setUploads((current) =>
          current.map((item) =>
            item.id === id
              ? { ...item, status, ...(message === undefined ? {} : { error: message }) }
              : item,
          ),
        )
      try {
        const signed = await presignUpload({
          name: bucket,
          key: prefix + file.name,
          content_type: file.type === '' ? 'application/octet-stream' : file.type,
        }).unwrap()
        await putToPresignedURL(signed, file)
        settle('done')
      } catch (err) {
        settle(
          'error',
          err instanceof Error ? err.message : errorMessage(err as ApiError, 'The upload failed.'),
        )
      }
    }
    dispatch(bucketsApi.util.invalidateTags([{ type: 'BucketObjects', id: bucket }]))
  }

  async function download(key: string) {
    setActionError(undefined)
    try {
      const signed = await presignDownload({ name: bucket, key }).unwrap()
      startDownload(signed.url)
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  async function confirmDelete() {
    setActionError(undefined)
    try {
      await deleteObjects({ name: bucket, keys: selected.map((entry) => entry.key) }).unwrap()
      dispatch(
        notify({
          type: 'success',
          header: `${selected.length} item${selected.length === 1 ? '' : 's'} deleted`,
          content: bucket,
        }),
      )
      setSelected([])
      setDeleteOpen(false)
    } catch (err) {
      setActionError(err as ApiError)
      setDeleteOpen(false)
    }
  }

  const single = selected.length === 1 ? selected[0] : undefined
  const columns: ReadonlyArray<TableProps.ColumnDefinition<Entry>> = [
    {
      id: 'name',
      header: 'Name',
      isRowHeader: true,
      cell: (entry) =>
        entry.kind === 'folder' ? (
          <Button
            variant="inline-link"
            iconName="folder"
            onClick={() => open(entry.key)}
            ariaLabel={`Open folder ${relative(entry.key, prefix)}`}
          >
            {relative(entry.key, prefix)}
          </Button>
        ) : (
          relative(entry.key, prefix)
        ),
    },
    {
      id: 'type',
      header: 'Type',
      cell: (entry) => (entry.kind === 'folder' ? 'Folder' : 'Object'),
    },
    {
      id: 'size',
      header: 'Size',
      cell: (entry) => (entry.kind === 'folder' ? '—' : formatBytes(entry.size)),
    },
    {
      id: 'last_modified',
      header: 'Last modified',
      cell: (entry) =>
        entry.kind === 'folder' ? (
          '—'
        ) : (
          <span title={formatAbsoluteTime(entry.lastModified)}>
            {formatRelativeTime(entry.lastModified)}
          </span>
        ),
    },
  ]

  return (
    <SpaceBetween size="m">
      {isPlainHTTP() && (
        <Alert type="warning" header="This console is served over plain HTTP">
          Objects you upload or download, and the links that grant access to them, cross the network
          unencrypted. Put Kumo and the object storage server behind TLS before storing anything
          sensitive.
        </Alert>
      )}

      {actionError !== undefined && Object.keys(fieldErrors(actionError)).length === 0 && (
        <Alert
          type="error"
          header="The request was refused"
          dismissible
          onDismiss={() => setActionError(undefined)}
        >
          {errorMessage(actionError, 'The request could not be carried out.')}
        </Alert>
      )}

      {uploads.length > 0 && (
        <Alert
          type={uploads.some((item) => item.status === 'error') ? 'warning' : 'info'}
          header="Uploads"
          dismissible
          onDismiss={() => setUploads([])}
        >
          <SpaceBetween size="xxs">
            {uploads.map((item) => (
              <SpaceBetween key={item.id} direction="horizontal" size="xs">
                <StatusIndicator
                  type={
                    item.status === 'uploading'
                      ? 'in-progress'
                      : item.status === 'done'
                        ? 'success'
                        : 'error'
                  }
                >
                  {item.name}
                </StatusIndicator>
                {item.error !== undefined && (
                  <Box variant="small" color="text-status-error">
                    {item.error}
                  </Box>
                )}
              </SpaceBetween>
            ))}
          </SpaceBetween>
        </Alert>
      )}

      <Table
        items={entries}
        columnDefinitions={columns}
        trackBy="key"
        loading={isFetching && data === undefined}
        loadingText="Loading objects"
        selectionType={canWrite ? 'multi' : 'single'}
        selectedItems={selected}
        onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
        ariaLabels={{
          selectionGroupLabel: 'Object selection',
          allItemsSelectionLabel: () => 'Select everything in this folder',
          itemSelectionLabel: (_data, entry) => `Select ${relative(entry.key, prefix)}`,
        }}
        empty={
          error !== undefined ? (
            <Box textAlign="center" color="text-status-error" padding="m">
              {errorMessage(error, 'The objects could not be listed.')}
            </Box>
          ) : (
            <Box textAlign="center" padding="l">
              <SpaceBetween size="s" alignItems="center">
                <b>{prefix === '' ? 'This bucket is empty' : 'This folder is empty'}</b>
                {canWrite && (
                  <Box variant="p" color="text-status-inactive">
                    Upload files or create a folder.
                  </Box>
                )}
              </SpaceBetween>
            </Box>
          )
        }
        header={
          <SpaceBetween size="xs">
            <Header
              variant="h2"
              actions={
                <SpaceBetween direction="horizontal" size="xs">
                  <Button
                    iconName="refresh"
                    ariaLabel="Refresh objects"
                    loading={isFetching}
                    onClick={() => void refetch()}
                  />
                  <Button
                    disabled={single?.kind !== 'object'}
                    onClick={() => single && void download(single.key)}
                  >
                    Download
                  </Button>
                  <Button
                    disabled={!canWrite || selected.length === 0}
                    onClick={() => setDeleteOpen(true)}
                    data-testid="delete-objects"
                  >
                    Delete
                  </Button>
                  <Button disabled={!canWrite} onClick={() => setFolderOpen(true)}>
                    Create folder
                  </Button>
                  <Button
                    variant="primary"
                    iconName="upload"
                    disabled={!canWrite}
                    onClick={() => fileInput.current?.click()}
                  >
                    Upload
                  </Button>
                </SpaceBetween>
              }
            >
              Objects
            </Header>
            <BreadcrumbGroup
              ariaLabel="Folder path"
              items={pathItems(bucket, prefix)}
              onFollow={(event) => {
                event.preventDefault()
                open(event.detail.href)
              }}
            />
          </SpaceBetween>
        }
        pagination={
          <Pagination
            currentPageIndex={page}
            pagesCount={nextCursor === null ? page : page + 1}
            openEnd={nextCursor !== null}
            onChange={({ detail }) => goToPage(detail.currentPageIndex)}
            ariaLabels={{
              paginationLabel: 'Object pagination',
              nextPageLabel: 'Next page',
              previousPageLabel: 'Previous page',
              pageLabel: (n) => `Page ${n}`,
            }}
          />
        }
      />

      <input
        ref={fileInput}
        type="file"
        multiple
        hidden
        data-testid="upload-input"
        onChange={(event) => {
          const files = event.target.files
          if (files !== null && files.length > 0) {
            void upload(files)
          }
          event.target.value = ''
        }}
      />

      {isFolderOpen && (
        <CreateFolderModal bucket={bucket} prefix={prefix} onClose={() => setFolderOpen(false)} />
      )}

      {isDeleteOpen && (
        <Modal
          visible
          onDismiss={() => setDeleteOpen(false)}
          header={`Delete ${selected.length} item${selected.length === 1 ? '' : 's'}?`}
          closeAriaLabel="Close"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={() => setDeleteOpen(false)}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  loading={isDeleting}
                  onClick={() => void confirmDelete()}
                  data-testid="confirm-delete-objects"
                >
                  Delete
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="s">
            <ul aria-label="Items to delete">
              {selected.map((entry) => (
                <li key={entry.key}>{entry.key}</li>
              ))}
            </ul>
            {selected.some((entry) => entry.kind === 'folder') && (
              <Alert type="info">
                Deleting a folder removes only the folder itself. Objects inside it are kept.
              </Alert>
            )}
            <Box variant="p">
              On a versioned bucket, a delete keeps the previous version; otherwise it cannot be
              undone.
            </Box>
          </SpaceBetween>
        </Modal>
      )}
    </SpaceBetween>
  )
}

interface CreateFolderModalProps {
  bucket: string
  prefix: string
  onClose: () => void
}

/** CreateFolderModal creates a folder inside the one being browsed. */
function CreateFolderModal({ bucket, prefix, onClose }: CreateFolderModalProps) {
  const [createFolder, { isLoading }] = useCreateFolderMutation()
  const [name, setName] = useState('')
  const [error, setError] = useState<ApiError>(undefined)
  const [localError, setLocalError] = useState<string | undefined>(undefined)

  async function submit() {
    const trimmed = name.trim().replace(/\/+$/, '')
    if (trimmed === '' || trimmed.includes('/')) {
      setLocalError('Enter a folder name without slashes.')
      return
    }
    setLocalError(undefined)
    try {
      await createFolder({ name: bucket, prefix: `${prefix}${trimmed}/` }).unwrap()
      onClose()
    } catch (err) {
      setError(err as ApiError)
    }
  }

  return (
    <Modal
      visible
      onDismiss={onClose}
      header="Create folder"
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
              onClick={() => void submit()}
              data-testid="submit-create-folder"
            >
              Create folder
            </Button>
          </SpaceBetween>
        </Box>
      }
    >
      <SpaceBetween size="s">
        <FormField
          label="Folder name"
          description={prefix === '' ? 'At the top of the bucket.' : `Inside ${prefix}`}
          errorText={localError ?? fieldErrors(error)['prefix']}
        >
          <Input
            value={name}
            onChange={({ detail }) => setName(detail.value)}
            ariaLabel="Folder name"
            autoFocus
          />
        </FormField>
        {error !== undefined && fieldErrors(error)['prefix'] === undefined && (
          <Alert type="error">{errorMessage(error, 'The folder could not be created.')}</Alert>
        )}
      </SpaceBetween>
    </Modal>
  )
}

/** relative is a key's name inside the folder being browsed. */
function relative(key: string, prefix: string): string {
  return key.startsWith(prefix) ? key.slice(prefix.length) : key
}

/** pathItems is the breadcrumb of a folder: the bucket, then each folder. */
function pathItems(bucket: string, prefix: string) {
  const parts = prefix.split('/').filter((part) => part !== '')
  return [
    { text: bucket, href: '' },
    ...parts.map((part, index) => ({
      text: part,
      href: `${parts.slice(0, index + 1).join('/')}/`,
    })),
  ]
}
