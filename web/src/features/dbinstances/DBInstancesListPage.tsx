import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useCollection } from '@cloudscape-design/collection-hooks'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import ButtonDropdown from '@cloudscape-design/components/button-dropdown'
import CollectionPreferences, {
  type CollectionPreferencesProps,
} from '@cloudscape-design/components/collection-preferences'
import Header from '@cloudscape-design/components/header'
import Pagination from '@cloudscape-design/components/pagination'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Table from '@cloudscape-design/components/table'
import TextFilter from '@cloudscape-design/components/text-filter'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import {
  dbInstancesApi,
  pollInterval,
  useListDBInstancesQuery,
  useRebootDBInstanceMutation,
  useStartDBInstanceMutation,
  useStopDBInstanceMutation,
  type DBInstance,
} from '@/features/dbinstances/api'
import { columnDefinitions, columnDisplay, columnOptions } from '@/features/dbinstances/columns'
import { DatabasesGate, NoEngineImageAlert, useEngines } from '@/features/dbinstances/DatabasesGate'
import { DeleteDBInstanceModal } from '@/features/dbinstances/DeleteDBInstanceModal'
import { canDelete, canReboot, canStart, canStop } from '@/features/dbinstances/state'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, type ApiError } from '@/lib/apiError'

const pageSizes = [10, 20, 50]

const defaultPreferences: CollectionPreferencesProps.Preferences = {
  pageSize: 20,
  wrapLines: false,
  stripedRows: true,
  contentDisplay: columnDisplay,
}

/** DBInstancesListPage is the Databases → Databases page. */
export function DBInstancesListPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Databases', href: '/databases' },
      { text: 'Databases', href: '/databases' },
    ],
  })

  return (
    <DatabasesGate title="Databases">
      <DBInstancesTable />
    </DatabasesGate>
  )
}

/**
 * DBInstancesTable lists the managed Postgres servers on this node.
 *
 * It polls at 5 s while anything is moving — a create builds a guest and waits
 * for its engine, a stop waits for the guest — and at 30 s otherwise. A
 * `failed` database is polled too, because the reconciler publishes one whose
 * engine answered late.
 */
function DBInstancesTable() {
  const navigate = useNavigate()
  const dispatch = useAppDispatch()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { engines } = useEngines()
  const cached = useAppSelector(dbInstancesApi.endpoints.listDBInstances.select())
  const { data, isLoading, isFetching, error, refetch } = useListDBInstancesQuery(undefined, {
    pollingInterval: pollInterval(cached.data?.items),
  })
  const [preferences, setPreferences] = useState(defaultPreferences)
  const [pendingDelete, setPendingDelete] = useState<DBInstance | null>(null)

  const [stopDBInstance] = useStopDBInstanceMutation()
  const [startDBInstance] = useStartDBInstanceMutation()
  const [rebootDBInstance] = useRebootDBInstanceMutation()

  const dbInstances = data?.items ?? []

  const { items, actions, collectionProps, filterProps, filteredItemsCount, paginationProps } =
    useCollection(dbInstances, {
      filtering: { fields: ['identifier', 'endpoint', 'database_name'] },
      sorting: {},
      pagination: { pageSize: preferences.pageSize },
      selection: { trackBy: 'id' },
    })
  const selected = collectionProps.selectedItems?.[0]

  async function run(
    action: 'stop' | 'start' | 'reboot',
    item: DBInstance,
    trigger: (id: string) => { unwrap: () => Promise<DBInstance> },
  ) {
    try {
      await trigger(item.id).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header:
            action === 'stop'
              ? 'Database stopping'
              : action === 'start'
                ? 'Database starting'
                : 'Database rebooting',
          content: item.identifier,
        }),
      )
    } catch (err) {
      dispatch(
        notify({
          type: 'error',
          header: `The database could not be ${action === 'stop' ? 'stopped' : action === 'start' ? 'started' : 'rebooted'}`,
          content: errorMessage(err as ApiError, 'The action was refused.'),
        }),
      )
    }
  }

  const empty =
    dbInstances.length === 0 ? (
      <Box textAlign="center" padding="l">
        <SpaceBetween size="s" alignItems="center">
          <Header variant="h2">No databases</Header>
          <Box variant="p">
            A DB instance is a Postgres server Kumo runs for you, reachable from inside its VPC.
          </Box>
          {isAdmin && engines.length > 0 && (
            <Button onClick={() => navigate('/databases/create')}>Create database</Button>
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
      {engines.length === 0 && <NoEngineImageAlert />}

      <Table
        {...collectionProps}
        items={items}
        loading={isLoading}
        loadingText="Loading databases"
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
          selectionGroupLabel: 'Database selection',
          itemSelectionLabel: (_data, item) => `Select ${item.identifier}`,
        }}
        header={
          <Header
            variant="awsui-h1-sticky"
            counter={dbInstances.length > 0 ? `(${dbInstances.length})` : undefined}
            description="Managed Postgres servers. Each one runs on a virtual machine Kumo owns, reachable from inside its VPC."
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh databases"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <ButtonDropdown
                  disabled={!isAdmin || selected === undefined}
                  data-testid="db-instance-actions"
                  items={[
                    {
                      id: 'stop',
                      text: 'Stop',
                      disabled: selected === undefined || !canStop(selected.state),
                    },
                    {
                      id: 'start',
                      text: 'Start',
                      disabled: selected === undefined || !canStart(selected.state),
                    },
                    {
                      id: 'reboot',
                      text: 'Reboot',
                      disabled: selected === undefined || !canReboot(selected.state),
                    },
                    {
                      id: 'delete',
                      text: 'Delete',
                      disabled: selected === undefined || !canDelete(selected.state),
                    },
                  ]}
                  onItemClick={({ detail }) => {
                    if (selected === undefined) {
                      return
                    }
                    switch (detail.id) {
                      case 'stop':
                        void run('stop', selected, stopDBInstance)
                        break
                      case 'start':
                        void run('start', selected, startDBInstance)
                        break
                      case 'reboot':
                        void run('reboot', selected, rebootDBInstance)
                        break
                      case 'delete':
                        setPendingDelete(selected)
                        break
                    }
                  }}
                >
                  Actions
                </ButtonDropdown>
                <Button
                  variant="primary"
                  disabled={!isAdmin || engines.length === 0}
                  onClick={() => navigate('/databases/create')}
                  data-testid="create-db-instance"
                >
                  Create database
                </Button>
              </SpaceBetween>
            }
          >
            Databases
          </Header>
        }
        filter={
          <TextFilter
            {...filterProps}
            filteringAriaLabel="Filter databases"
            filteringPlaceholder="Find databases"
            countText={`${filteredItemsCount ?? 0} matches`}
          />
        }
        pagination={
          <Pagination
            {...paginationProps}
            ariaLabels={{ paginationLabel: 'Database pagination' }}
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
              title: 'Databases per page',
              options: pageSizes.map((size) => ({ value: size, label: `${size} databases` })),
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
          <Alert type="error" header="The databases could not be loaded">
            {errorMessage(error, 'The databases could not be loaded.')}
          </Alert>
        </Box>
      )}

      {pendingDelete !== null && (
        <DeleteDBInstanceModal
          dbInstance={pendingDelete}
          onClose={() => setPendingDelete(null)}
          onDeleted={() => actions.setSelectedItems([])}
        />
      )}
    </>
  )
}
