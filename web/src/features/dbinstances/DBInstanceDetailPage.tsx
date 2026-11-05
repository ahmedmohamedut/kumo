import { useMemo, useState, type ReactNode } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import ColumnLayout from '@cloudscape-design/components/column-layout'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import CopyToClipboard from '@cloudscape-design/components/copy-to-clipboard'
import Header from '@cloudscape-design/components/header'
import Link from '@cloudscape-design/components/link'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import Table from '@cloudscape-design/components/table'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import {
  dbInstancesApi,
  pollInterval,
  useGetDBInstanceQuery,
  useRebootDBInstanceMutation,
  useStartDBInstanceMutation,
  useStopDBInstanceMutation,
  type DBInstance,
} from '@/features/dbinstances/api'
import { DatabasesGate } from '@/features/dbinstances/DatabasesGate'
import { DeleteDBInstanceModal } from '@/features/dbinstances/DeleteDBInstanceModal'
import {
  canDelete,
  canReboot,
  canStart,
  canStop,
  connectionURI,
  DB_INSTANCE_INDICATOR,
  psqlExample,
} from '@/features/dbinstances/state'
import { notify } from '@/features/notifications/notificationsSlice'
import {
  pollInterval as taskPollInterval,
  tasksApi,
  useListTasksQuery,
  type Task,
} from '@/features/tasks/api'
import { taskIndicatorType, taskLabel } from '@/features/tasks/state'
import { errorMessage, isNotFound, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/** DBInstanceDetailPage is one database's page. */
export function DBInstanceDetailPage() {
  const { dbInstanceId } = useParams<{ dbInstanceId: string }>()
  const id = dbInstanceId ?? ''

  usePageMeta({
    breadcrumbs: [
      { text: 'Databases', href: '/databases' },
      { text: 'Databases', href: '/databases' },
      { text: id, href: `/databases/${id}` },
    ],
  })

  if (id === '') {
    return null
  }
  return (
    <DatabasesGate title={id}>
      <DBInstanceDetail id={id} />
    </DatabasesGate>
  )
}

/**
 * DBInstanceDetail shows one database: its state, how to connect to it, the
 * server behind it and the tasks that worked on it.
 *
 * The Connect panel is the point of the page. The password is not on it and
 * cannot be: Kumo discarded it when the database was created, which is why the
 * `psql` example leaves it to be prompted for.
 */
function DBInstanceDetail({ id }: { id: string }) {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)

  const cached = useAppSelector(dbInstancesApi.endpoints.getDBInstance.select(id))
  const taskArgs = useMemo(() => ({ resourceId: id }), [id])
  const cachedTasks = useAppSelector(tasksApi.endpoints.listTasks.select(taskArgs))

  const {
    data: dbInstance,
    isLoading,
    isFetching,
    error,
    refetch,
  } = useGetDBInstanceQuery(id, {
    pollingInterval: pollInterval(cached.data === undefined ? undefined : [cached.data]),
  })
  const { data: tasksData } = useListTasksQuery(taskArgs, {
    pollingInterval: taskPollInterval(cachedTasks?.data?.items),
  })

  const [stopDBInstance, { isLoading: isStopping }] = useStopDBInstanceMutation()
  const [startDBInstance, { isLoading: isStarting }] = useStartDBInstanceMutation()
  const [rebootDBInstance, { isLoading: isRebooting }] = useRebootDBInstanceMutation()

  const [isDeleteOpen, setDeleteOpen] = useState(false)
  const [actionError, setActionError] = useState<ApiError>(undefined)

  const tasks = tasksData?.items ?? []

  async function act(
    label: string,
    trigger: (id: string) => { unwrap: () => Promise<DBInstance> },
  ) {
    setActionError(undefined)
    try {
      await trigger(id).unwrap()
      dispatch(
        notify({ type: 'in-progress', header: label, content: dbInstance?.identifier ?? id }),
      )
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  if (isLoading) {
    return <Spinner size="large" />
  }

  if (error !== undefined && isNotFound(error)) {
    return (
      <ContentLayout header={<Header variant="h1">{id}</Header>}>
        <Alert
          type="info"
          header="This database is gone"
          action={<Button onClick={() => navigate('/databases')}>Back to databases</Button>}
        >
          It has been deleted, with its storage. Nothing on this node can recover it.
        </Alert>
      </ContentLayout>
    )
  }

  if (dbInstance === undefined) {
    return (
      <ContentLayout header={<Header variant="h1">{id}</Header>}>
        <Alert type="error" header="The database could not be loaded">
          {errorMessage(error, 'The database could not be loaded.')}
        </Alert>
      </ContentLayout>
    )
  }

  const indicator = DB_INSTANCE_INDICATOR[dbInstance.state]
  const deleting = dbInstance.state === 'deleting'
  const canAct = isAdmin && !deleting

  return (
    <>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description={`PostgreSQL ${dbInstance.engine_version} on ${dbInstance.instance_type}`}
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh database"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <Button
                  disabled={!canAct || !canStop(dbInstance.state)}
                  loading={isStopping}
                  onClick={() => void act('Database stopping', stopDBInstance)}
                  data-testid="stop-db-instance"
                >
                  Stop
                </Button>
                <Button
                  disabled={!canAct || !canStart(dbInstance.state)}
                  loading={isStarting}
                  onClick={() => void act('Database starting', startDBInstance)}
                  data-testid="start-db-instance"
                >
                  Start
                </Button>
                <Button
                  disabled={!canAct || !canReboot(dbInstance.state)}
                  loading={isRebooting}
                  onClick={() => void act('Database rebooting', rebootDBInstance)}
                  data-testid="reboot-db-instance"
                >
                  Reboot
                </Button>
                <Button
                  disabled={!isAdmin || !canDelete(dbInstance.state)}
                  onClick={() => setDeleteOpen(true)}
                  data-testid="delete-db-instance"
                >
                  Delete
                </Button>
              </SpaceBetween>
            }
          >
            {dbInstance.identifier}
          </Header>
        }
      >
        <SpaceBetween size="l">
          {actionError !== undefined && (
            <Alert type="error" header="The action was refused">
              {errorMessage(actionError, 'The action could not be carried out.')}
            </Alert>
          )}

          {dbInstance.state === 'failed' && (
            <Alert type="error" header="This database is not answering">
              <SpaceBetween size="xs">
                <span>{dbInstance.state_reason ?? 'The engine did not answer.'}</span>
                <span>
                  The server is left running so it can be looked at. A reboot is worth trying; the
                  guest's console log is under{' '}
                  <Box variant="code">$KUMO_DATA_DIR/console/{dbInstance.instance_id}.log</Box> on
                  the node. If it cannot be recovered, delete it and create it again.
                </span>
              </SpaceBetween>
            </Alert>
          )}

          {dbInstance.state === 'creating' && (
            <Alert type="info" header="This database is being created">
              The server is booting and creating its cluster. It becomes available once the engine
              answers on its address, usually in about a minute.
            </Alert>
          )}

          <Container header={<Header variant="h2">Details</Header>}>
            <ColumnLayout columns={4} variant="text-grid">
              <Row label="Status">
                <StatusIndicator type={indicator.type}>{indicator.label}</StatusIndicator>
              </Row>
              <Row label="Engine">PostgreSQL {dbInstance.engine_version}</Row>
              <Row label="Size">{dbInstance.instance_type}</Row>
              <Row label="Compute">
                {dbInstance.vcpus !== undefined && dbInstance.memory_mib !== undefined
                  ? `${dbInstance.vcpus} vCPU, ${dbInstance.memory_mib} MiB`
                  : '—'}
              </Row>
              <Row label="Storage">{dbInstance.storage_gb} GiB</Row>
              <Row label="Created">
                <span title={formatAbsoluteTime(dbInstance.created_at)}>
                  {formatRelativeTime(dbInstance.created_at)}
                </span>
              </Row>
              <Row label="Server">
                {/* The VM is Kumo's, not the user's: it is on no Instances page
                    and its own endpoints refuse every write. The id is here
                    because the console log and the orphan report are keyed by
                    it (07-databases.md section 8). */}
                <Box variant="code">{dbInstance.instance_id}</Box>
              </Row>
              <Row label="DB instance ID">
                <Box variant="code">{dbInstance.id}</Box>
              </Row>
            </ColumnLayout>
          </Container>

          <Container
            header={
              <Header
                variant="h2"
                description="Reachable from instances in the same VPC. Nothing outside the VPC can connect."
              >
                Connect
              </Header>
            }
          >
            <SpaceBetween size="m">
              <Alert type="warning" header="Connections are not encrypted">
                This release has no TLS, so the VPC is the boundary:{' '}
                <Box variant="code">sslmode=require</Box> will not connect, and the password is sent
                over the network protected only by SCRAM.
              </Alert>

              <ColumnLayout columns={4} variant="text-grid">
                <Row label="Endpoint">
                  {dbInstance.endpoint === null || dbInstance.endpoint === undefined ? (
                    <Box variant="span" color="text-status-inactive">
                      Not yet assigned
                    </Box>
                  ) : (
                    <CopyToClipboard
                      variant="inline"
                      textToCopy={dbInstance.endpoint}
                      copyButtonAriaLabel="Copy endpoint"
                      copySuccessText="Endpoint copied"
                      copyErrorText="Endpoint not copied"
                    />
                  )}
                </Row>
                <Row label="Port">{dbInstance.port}</Row>
                <Row label="Database">
                  <Box variant="code">{dbInstance.database_name}</Box>
                </Row>
                <Row label="Admin user">
                  <Box variant="code">{dbInstance.admin_username}</Box>
                </Row>
              </ColumnLayout>

              <Box>
                <Box variant="awsui-key-label">psql</Box>
                <CopyToClipboard
                  variant="inline"
                  textToCopy={psqlExample(dbInstance)}
                  copyButtonAriaLabel="Copy psql command"
                  copySuccessText="Command copied"
                  copyErrorText="Command not copied"
                />
              </Box>

              <Box>
                <Box variant="awsui-key-label">Connection URI</Box>
                <CopyToClipboard
                  variant="inline"
                  textToCopy={connectionURI(dbInstance)}
                  copyButtonAriaLabel="Copy connection URI"
                  copySuccessText="URI copied"
                  copyErrorText="URI not copied"
                />
              </Box>

              <Box variant="small">
                Neither carries the password: Kumo derived the engine's credential from it and
                discarded it, so it cannot be shown again or reset. <Box variant="code">psql</Box>{' '}
                prompts for it.
              </Box>
            </SpaceBetween>
          </Container>

          <Container
            header={
              <Header variant="h2" description="The operations Kumo has run on this database.">
                Activity
              </Header>
            }
          >
            <Table
              items={tasks}
              variant="embedded"
              empty={
                <Box textAlign="center" padding="s" color="text-status-inactive">
                  No tasks yet
                </Box>
              }
              columnDefinitions={[
                { id: 'type', header: 'Operation', cell: (task: Task) => task.type },
                {
                  id: 'state',
                  header: 'Status',
                  cell: (task: Task) => (
                    <StatusIndicator type={taskIndicatorType(task.state)}>
                      {taskLabel(task.state)}
                    </StatusIndicator>
                  ),
                },
                {
                  id: 'created_at',
                  header: 'Started',
                  cell: (task: Task) => (
                    <span title={formatAbsoluteTime(task.created_at)}>
                      {formatRelativeTime(task.created_at)}
                    </span>
                  ),
                },
                {
                  id: 'error',
                  header: 'Detail',
                  cell: (task: Task) => task.error ?? '—',
                },
              ]}
              ariaLabels={{ tableLabel: 'Database activity' }}
            />
          </Container>

          {!isAdmin && (
            <Alert type="info">
              A viewer can read a database and its connection details. Stopping, starting, rebooting
              and deleting are administrator actions.
            </Alert>
          )}

          <Box>
            <Link href="/databases">Back to databases</Link>
          </Box>
        </SpaceBetween>
      </ContentLayout>

      {isDeleteOpen && (
        <DeleteDBInstanceModal
          dbInstance={dbInstance}
          onClose={() => setDeleteOpen(false)}
          onDeleted={() => navigate('/databases')}
        />
      )}
    </>
  )
}

/** Row is one label/value pair of a detail grid. */
function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <Box variant="awsui-key-label">{label}</Box>
      <div>{children}</div>
    </div>
  )
}
