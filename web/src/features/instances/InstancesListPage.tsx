import { useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useCollection } from '@cloudscape-design/collection-hooks'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import ButtonDropdown from '@cloudscape-design/components/button-dropdown'
import CollectionPreferences from '@cloudscape-design/components/collection-preferences'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import Modal from '@cloudscape-design/components/modal'
import Pagination from '@cloudscape-design/components/pagination'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Table from '@cloudscape-design/components/table'
import TextFilter from '@cloudscape-design/components/text-filter'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import {
  ACTIONS,
  ACTIONS_NEEDING_CONFIRMATION,
  ACTION_LABELS,
  canPerform,
  isForce,
  type InstanceAction,
} from '@/features/instances/actions'
import {
  columnDisplayPreferences,
  columnOptions,
  defaultColumns,
  type Instance,
} from '@/features/instances/columns'
import {
  instancesApi,
  pollInterval,
  useListInstancesQuery,
  useRebootInstanceMutation,
  useStartInstanceMutation,
  useStopInstanceMutation,
  useTerminateInstanceMutation,
} from '@/features/instances/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'

/** pageSizes are the table page sizes offered in the preferences panel. */
const pageSizes = [20, 50, 100]

/** preferences are the table's page size and column visibility. */
const preferences = {
  pageSize: 20,
  pageSizes,
  wrapLines: false,
  stripedRows: true,
  contentDisplay: [
    { id: 'name', label: 'Name', visible: true },
    { id: 'id', label: 'Instance ID', visible: true },
    { id: 'state', label: 'State', visible: true },
    { id: 'instance_type', label: 'Instance type', visible: true },
    { id: 'private_ip', label: 'Private IP', visible: true },
    { id: 'image_name', label: 'Image', visible: true },
    { id: 'key_pair_name', label: 'Key pair', visible: true },
    { id: 'root_disk_gb', label: 'Root disk', visible: true },
    { id: 'launched_at', label: 'Launched', visible: true },
  ],
  wrapLinesEnabled: true,
  stripedRowsEnabled: true,
}

/**
 * A confirmation an operator has to accept before something is destroyed.
 *
 * `typedName` is only demanded for terminate: the name is short, and typing it is
 * the difference between an operator who meant to remove `web-1` and one who meant
 * to remove whichever row happened to be selected when they reached for the mouse.
 */
interface PendingAction {
  action: InstanceAction
  instance: Instance
  typedName: string
}

/**
 * InstancesListPage lists the instances on this node and runs the lifecycle
 * actions on them.
 *
 * Filtering runs on the server (`?q=`) rather than in `useCollection`, so a search
 * covers every instance instead of only the page that happens to be loaded; the
 * hook then handles sorting and paging over the page it got back. The filter lives
 * in the URL, so a filtered list is linkable.
 *
 * The poll interval follows the data: 5 s while an instance is launching,
 * stopping or terminating, 30 s once everything has settled (03-frontend.md §5).
 * `running` is not transitional — a page of long-lived VMs should not be polled
 * five times a second just because they are up.
 *
 * Every action answers `202` and queues a task, so nothing here waits on a VM. The
 * row moves to `stopping`/`terminating` on the next poll, and the dropdown for the
 * new state decides what can come next.
 */
export function InstancesListPage() {
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
  const cached = useAppSelector(instancesApi.endpoints.listInstances.select(args))
  const { data, isLoading, isFetching, error, refetch } = useListInstancesQuery(args, {
    pollingInterval: pollInterval(cached.data?.items),
  })

  const [startInstance] = useStartInstanceMutation()
  const [stopInstance, { isLoading: isStopping }] = useStopInstanceMutation()
  const [rebootInstance, { isLoading: isRebooting }] = useRebootInstanceMutation()
  const [terminateInstance, { isLoading: isTerminating }] = useTerminateInstanceMutation()

  const [pending, setPending] = useState<PendingAction | null>(null)
  const [actionError, setActionError] = useState<ApiError>(undefined)

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Instances', href: '/instances' },
    ],
  })

  const items = data?.items ?? []

  const {
    items: visibleItems,
    collectionProps,
    filterProps,
    paginationProps,
  } = useCollection(items, {
    filtering: { defaultFilteringText: query, fields: ['name', 'id'] },
    pagination: { pageSize: preferences.pageSize },
    // `selectionType` is a Table prop, not a collection-hook option: the hook only
    // tracks which items are picked. Every action here is a power operation on one
    // guest, so the Table is told to allow a single row below.
    selection: {},
  })

  const { selectedItems } = collectionProps
  const selected = selectedItems?.[0]

  /** run fires an action that needs no confirmation. */
  async function run(action: InstanceAction, instance: Instance) {
    setActionError(undefined)
    try {
      switch (action) {
        case 'start':
          await startInstance(instance.id).unwrap()
          break
        case 'stop':
        case 'force-stop':
          await stopInstance({ id: instance.id, force: isForce(action) }).unwrap()
          break
        case 'reboot':
          await rebootInstance({ id: instance.id, force: false }).unwrap()
          break
        case 'terminate':
          await terminateInstance(instance.id).unwrap()
          break
      }
      dispatch(
        notify({
          type: 'success',
          header: `${ACTION_LABELS[action]} started`,
          // The API answers 202 with a task, so this is a request accepted, not an
          // outcome. Saying so keeps the row's next state from reading as a lie.
          content: `${instance.name}: ${ACTION_LABELS[action].toLowerCase()} is running in the background.`,
        }),
      )
    } catch (err) {
      setActionError(err as ApiError)
      dispatch(
        notify({
          type: 'error',
          header: `${ACTION_LABELS[action]} failed`,
          content: errorMessage(
            err as ApiError,
            `The instance could not be ${ACTION_LABELS[action].toLowerCase()}d.`,
          ),
        }),
      )
    }
  }

  /** onAction routes through the confirmation when the action needs one. */
  function onAction(action: InstanceAction) {
    if (selected === undefined) {
      return
    }
    setActionError(undefined)
    if (ACTIONS_NEEDING_CONFIRMATION.has(action)) {
      setPending({ action, instance: selected, typedName: '' })
      return
    }
    void run(action, selected)
  }

  // Only terminate demands the name. Force stop destroys work but not the
  // instance, so asking the operator to type the name there would be a ritual with
  // no added safety — and it would leave the confirm button permanently shut,
  // because the dialog for force stop has no field to fill it from.
  const pendingNameMatches =
    pending !== null &&
    (pending.action !== 'terminate' || pending.typedName === pending.instance.name)
  const isActing = isStopping || isRebooting || isTerminating || pending !== null

  return (
    <>
      <Table
        {...collectionProps}
        items={visibleItems}
        loading={isLoading}
        loadingText="Loading instances"
        columnDefinitions={defaultColumns}
        columnDisplay={columnDisplayPreferences}
        wrapLines={preferences.wrapLines}
        stripedRows={preferences.stripedRows}
        // Every action is a power operation on one guest and the API takes one id,
        // so multi-select would offer choices that cannot be carried out.
        selectionType="single"
        trackBy="id"
        variant="full-page"
        header={
          <Header
            counter={items.length > 0 ? `(${items.length})` : undefined}
            description="Virtual machines on this node."
            actions={
              <SpaceBetween direction="horizontal" size="s">
                <ButtonDropdown
                  disabled={selected === undefined || isActing}
                  items={ACTIONS.map((action) => ({
                    id: action,
                    text: ACTION_LABELS[action],
                    // Disabled rather than hidden: an operator looking at a
                    // stopped instance should be able to see that Start exists and
                    // why it is not the thing they can press right now.
                    disabled: selected !== undefined && !canPerform(action, selected.state),
                  }))}
                  onItemClick={({ detail }) => onAction(detail.id as InstanceAction)}
                >
                  Instance state
                </ButtonDropdown>
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh instances"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <Button variant="primary" onClick={() => navigate('/instances/launch')}>
                  Launch instance
                </Button>
              </SpaceBetween>
            }
          >
            Instances
          </Header>
        }
        filter={
          <TextFilter
            {...filterProps}
            filteringAriaLabel="Filter instances"
            filteringPlaceholder="Find instances"
            countText={`${items.length} instances`}
            onChange={({ detail }) => {
              // Cloudscape's ChangeDetail and the collection hook's event shape are
              // the same object, so hand the hook the wrapper it expects.
              filterProps.onChange({ detail })
              const next = detail.filteringText
              // Keep the filter in the URL so the list is linkable and the reload
              // does not silently widen the search.
              setSearchParams(next === '' ? {} : { q: next }, { replace: true })
            }}
          />
        }
        pagination={
          <Pagination
            {...paginationProps}
            ariaLabels={{ paginationLabel: 'Instance pagination' }}
          />
        }
        preferences={
          <CollectionPreferences
            title="Preferences"
            confirmLabel="Confirm"
            cancelLabel="Cancel"
            preferences={preferences}
            pageSizePreference={{
              title: 'Instances per page',
              options: pageSizes.map((size) => ({
                value: size,
                label: `${size} instances`,
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
                {query === '' ? 'No instances' : `No instances match "${query}"`}
              </Header>
              {query === '' ? (
                <>
                  <Box variant="p">
                    Launch an instance from an image you have imported to this node.
                  </Box>
                  <Button variant="primary" onClick={() => navigate('/instances/launch')}>
                    Launch instance
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
          <Alert type="error" header="The instances could not be loaded">
            {errorMessage(error, 'The instances could not be loaded.')}
          </Alert>
        </Box>
      )}

      {pending !== null && (
        <Modal
          visible
          onDismiss={() => setPending(null)}
          header={
            pending.action === 'terminate'
              ? `Terminate ${pending.instance.name}?`
              : `Force stop ${pending.instance.name}?`
          }
          closeAriaLabel="Close"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="s">
                <Button variant="link" onClick={() => setPending(null)}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  loading={isTerminating || isStopping}
                  // Terminate additionally requires the name, so a mis-click on
                  // the wrong row cannot delete the wrong guest's disks.
                  disabled={!pendingNameMatches}
                  onClick={() => {
                    const target = pending.instance
                    const action = pending.action
                    setPending(null)
                    void run(action, target)
                  }}
                  data-testid="confirm-destructive"
                >
                  {ACTION_LABELS[pending.action]}
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Alert type="warning" header="This cannot be undone">
              {pending.action === 'terminate'
                ? 'Terminating removes the domain and both of its disks, and then the instance itself: the row is deleted, the name is freed, and only the tasks are left to show what happened.'
                : 'A force stop cuts power to the guest. Anything it has not written to disk is lost, exactly as if the machine had been unplugged.'}
            </Alert>

            {actionError !== undefined && (
              <Alert type="error" header="The action failed">
                {errorMessage(actionError, 'The action could not be started.')}
              </Alert>
            )}

            {pending.action === 'terminate' && (
              <FormField
                label="Type the name to confirm"
                description={`Enter "${pending.instance.name}" to enable the button.`}
              >
                <Input
                  value={pending.typedName}
                  onChange={({ detail }) =>
                    setPending((current) =>
                      current === null ? null : { ...current, typedName: detail.value },
                    )
                  }
                  ariaLabel="Type the name to confirm"
                />
              </FormField>
            )}
          </SpaceBetween>
        </Modal>
      )}
    </>
  )
}
