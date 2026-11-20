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
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import Table from '@cloudscape-design/components/table'
import Tabs from '@cloudscape-design/components/tabs'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import { notify } from '@/features/notifications/notificationsSlice'
import {
  pollInterval,
  sparkApi,
  useGetSparkClusterQuery,
  useSparkClientConfigMutation,
  type SparkCluster,
  type SparkNode,
  type SparkNodeGroup,
} from '@/features/spark/api'
import { DeleteSparkClusterModal } from '@/features/spark/DeleteSparkClusterModal'
import { ResizeSparkNodeGroupModal } from '@/features/spark/ResizeSparkNodeGroupModal'
import { SparkGate } from '@/features/spark/SparkGate'
import {
  aliveCount,
  canDelete,
  canDownloadConfig,
  canScale,
  SPARK_INDICATOR,
  submitExample,
} from '@/features/spark/state'
import {
  pollInterval as taskPollInterval,
  tasksApi,
  useListTasksQuery,
  type Task,
} from '@/features/tasks/api'
import { taskIndicatorType, taskLabel } from '@/features/tasks/state'
import { errorMessage, isNotFound, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/** SparkClusterDetailPage is one Spark cluster's page. */
export function SparkClusterDetailPage() {
  const { clusterId } = useParams<{ clusterId: string }>()
  const id = clusterId ?? ''
  usePageMeta({
    breadcrumbs: [
      { text: 'Spark', href: '/spark' },
      { text: 'Clusters', href: '/spark' },
      { text: id, href: `/spark/${id}` },
    ],
  })
  if (id === '') {
    return null
  }
  return (
    <SparkGate title={id}>
      <SparkClusterDetail id={id} />
    </SparkGate>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <Box variant="awsui-key-label">{label}</Box>
      <div>{children}</div>
    </div>
  )
}

function dash() {
  return (
    <Box variant="span" color="text-status-inactive">
      —
    </Box>
  )
}

/** saveFile hands the browser a file to download, where it can. */
function saveFile(name: string, content: string) {
  if (typeof URL.createObjectURL !== 'function') {
    return
  }
  const url = URL.createObjectURL(new Blob([content], { type: 'text/plain' }))
  const link = document.createElement('a')
  link.href = url
  link.download = name
  link.click()
  URL.revokeObjectURL(url)
}

function SparkClusterDetail({ id }: { id: string }) {
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const cached = useAppSelector(sparkApi.endpoints.getSparkCluster.select(id))
  const taskArgs = useMemo(() => ({ resourceId: id }), [id])
  const cachedTasks = useAppSelector(tasksApi.endpoints.listTasks.select(taskArgs))
  const {
    data: cluster,
    isLoading,
    isFetching,
    error,
    refetch,
  } = useGetSparkClusterQuery(id, {
    pollingInterval: pollInterval(cached.data === undefined ? undefined : [cached.data]),
  })
  const { data: tasksData } = useListTasksQuery(taskArgs, {
    pollingInterval: taskPollInterval(cachedTasks?.data?.items),
  })
  const [isDeleteOpen, setDeleteOpen] = useState(false)
  const [resizing, setResizing] = useState<SparkNodeGroup | null>(null)

  if (isLoading) {
    return <Spinner size="large" />
  }
  if (error !== undefined && isNotFound(error)) {
    return (
      <ContentLayout header={<Header variant="h1">{id}</Header>}>
        <Alert
          type="info"
          header="This cluster is gone"
          action={<Button onClick={() => navigate('/spark')}>Back to clusters</Button>}
        >
          It has been deleted, with its nodes. Its buckets and event logs are kept.
        </Alert>
      </ContentLayout>
    )
  }
  if (cluster === undefined) {
    return (
      <ContentLayout header={<Header variant="h1">{id}</Header>}>
        <Alert type="error" header="The cluster could not be loaded">
          {errorMessage(error, 'The cluster could not be loaded.')}
        </Alert>
      </ContentLayout>
    )
  }

  const { alive, total } = aliveCount(cluster)

  return (
    <>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description={`Spark ${cluster.spark_version}, ${total} workers`}
            actions={
              <SpaceBetween direction="horizontal" size="xs">
                <Button
                  iconName="refresh"
                  ariaLabel="Refresh cluster"
                  loading={isFetching}
                  onClick={() => void refetch()}
                />
                <Button
                  disabled={!isAdmin || !canDelete(cluster.state)}
                  onClick={() => setDeleteOpen(true)}
                  data-testid="delete-spark-cluster"
                >
                  Delete
                </Button>
              </SpaceBetween>
            }
          >
            {cluster.name}
          </Header>
        }
      >
        <SpaceBetween size="l">
          {cluster.state === 'failed' && (
            <Alert type="error" header="This cluster did not come up">
              <SpaceBetween size="xs">
                <span>{cluster.state_reason ?? 'The master did not answer.'}</span>
                <span>
                  The nodes are kept so they can be looked at: each one&apos;s console log is under{' '}
                  <Box variant="code">$KUMO_DATA_DIR/console/&lt;instance id&gt;.log</Box> on the
                  node. Delete the cluster to remove them.
                </span>
              </SpaceBetween>
            </Alert>
          )}
          {cluster.state === 'degraded' && (
            <Alert type="warning" header="This cluster is degraded">
              {cluster.state_reason ?? 'Something on the cluster is not healthy.'} Its master
              answers; it returns to active on its own once the cause is gone.
            </Alert>
          )}
          {cluster.state === 'creating' && (
            <Alert type="info" header="This cluster is being created">
              The master boots first, then the workers register with it. The cluster is active once
              every worker has.
            </Alert>
          )}

          <Tabs
            ariaLabel="Spark cluster"
            tabs={[
              {
                id: 'overview',
                label: 'Overview',
                content: (
                  <Overview
                    cluster={cluster}
                    alive={alive}
                    total={total}
                    isAdmin={isAdmin}
                    onResize={setResizing}
                    tasks={tasksData?.items ?? []}
                  />
                ),
              },
              {
                id: 'nodes',
                label: `Nodes (${cluster.nodes.length})`,
                content: <Nodes nodes={cluster.nodes} />,
              },
              { id: 'connect', label: 'Connect', content: <Connect cluster={cluster} /> },
            ]}
          />
        </SpaceBetween>
      </ContentLayout>

      {isDeleteOpen && (
        <DeleteSparkClusterModal
          cluster={cluster}
          onClose={() => setDeleteOpen(false)}
          onDeleted={() => navigate('/spark')}
        />
      )}
      {resizing !== null && (
        <ResizeSparkNodeGroupModal
          cluster={cluster}
          nodeGroup={resizing}
          onClose={() => setResizing(null)}
        />
      )}
    </>
  )
}

function Overview({
  cluster,
  alive,
  total,
  isAdmin,
  onResize,
  tasks,
}: {
  cluster: SparkCluster
  alive: number
  total: number
  isAdmin: boolean
  onResize: (group: SparkNodeGroup) => void
  tasks: Task[]
}) {
  const indicator = SPARK_INDICATOR[cluster.state]
  return (
    <SpaceBetween size="l">
      <Container header={<Header variant="h2">Details</Header>}>
        <ColumnLayout columns={4} variant="text-grid">
          <Row label="Status">
            <StatusIndicator type={indicator.type}>{indicator.label}</StatusIndicator>
          </Row>
          <Row label="Spark">{cluster.spark_version}</Row>
          <Row label="Workers alive">
            {alive} / {total}
          </Row>
          <Row label="Master size">{cluster.master_type}</Row>
          <Row label="Object storage">
            {cluster.object_storage ? cluster.buckets.join(', ') : 'Off'}
          </Row>
          <Row label="Event logs">
            {cluster.event_log_bucket === null || cluster.event_log_bucket === undefined
              ? 'Off'
              : `s3a://${cluster.event_log_bucket}/spark-events/${cluster.id}`}
          </Row>
          <Row label="Created">
            <span title={formatAbsoluteTime(cluster.created_at)}>
              {formatRelativeTime(cluster.created_at)}
            </span>
          </Row>
          <Row label="Cluster ID">
            <Box variant="code">{cluster.id}</Box>
          </Row>
        </ColumnLayout>
      </Container>

      <Table
        items={cluster.node_groups}
        variant="container"
        header={
          <Header variant="h2" description="The cluster's workers.">
            Node group
          </Header>
        }
        columnDefinitions={[
          { id: 'name', header: 'Name', cell: (g: SparkNodeGroup) => g.name },
          { id: 'type', header: 'Size', cell: (g: SparkNodeGroup) => g.instance_type },
          { id: 'desired', header: 'Workers', cell: (g: SparkNodeGroup) => g.desired_size },
          {
            id: 'actions',
            header: 'Actions',
            cell: (g: SparkNodeGroup) => (
              <Button
                variant="inline-link"
                disabled={!isAdmin || !canScale(cluster.state)}
                onClick={() => onResize(g)}
                data-testid="resize-spark-node-group"
              >
                Resize
              </Button>
            ),
          },
        ]}
        ariaLabels={{ tableLabel: 'Node group' }}
      />

      <Container header={<Header variant="h2">Activity</Header>}>
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
            { id: 'error', header: 'Detail', cell: (task: Task) => task.error ?? '—' },
          ]}
          ariaLabels={{ tableLabel: 'Cluster activity' }}
        />
      </Container>
    </SpaceBetween>
  )
}

function Nodes({ nodes }: { nodes: SparkNode[] }) {
  return (
    <Table
      items={nodes}
      variant="container"
      header={
        <Header
          variant="h2"
          description="The virtual machines behind the cluster. They are Kumo's, and are not on the Instances pages."
        >
          Nodes
        </Header>
      }
      columnDefinitions={[
        { id: 'name', header: 'Name', isRowHeader: true, cell: (n: SparkNode) => n.name },
        {
          id: 'role',
          header: 'Role',
          cell: (n: SparkNode) => (n.role === 'master' ? 'Master' : 'Worker'),
        },
        {
          id: 'alive',
          header: 'Spark',
          cell: (n: SparkNode) =>
            n.alive === null || n.alive === undefined ? (
              <StatusIndicator type="pending">Not yet seen</StatusIndicator>
            ) : n.alive ? (
              <StatusIndicator type="success">Alive</StatusIndicator>
            ) : (
              <StatusIndicator type="warning">Not registered</StatusIndicator>
            ),
        },
        { id: 'state', header: 'VM', cell: (n: SparkNode) => n.state },
        { id: 'ip', header: 'Address', cell: (n: SparkNode) => n.private_ip },
        {
          id: 'instance',
          header: 'Instance',
          cell: (n: SparkNode) => <Box variant="code">{n.instance_id}</Box>,
        },
      ]}
      ariaLabels={{ tableLabel: 'Nodes' }}
    />
  )
}

/**
 * Connect is the point of the page: the master URL, the web UIs, and for an
 * admin the client configuration, which carries the cluster's secret and S3
 * key (09-spark.md section 5, option A), downloaded on demand and stored
 * nowhere on the way.
 */
function Connect({ cluster }: { cluster: SparkCluster }) {
  const dispatch = useAppDispatch()
  const isAdmin = useAppSelector(selectIsAdmin)
  const [download, { isLoading }] = useSparkClientConfigMutation()
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<ApiError>(undefined)

  async function save() {
    setError(undefined)
    try {
      const conf = await download(cluster.id).unwrap()
      saveFile(conf.filename, conf.client_config)
      setSaved(true)
      dispatch(
        notify({
          type: 'success',
          header: 'Client configuration downloaded',
          content: `${conf.filename}: keep it like a password, it holds the cluster's secret.`,
        }),
      )
    } catch (err) {
      setError(err as ApiError)
    }
  }

  return (
    <Container
      header={
        <Header
          variant="h2"
          description="Reachable from instances in the cluster's VPC. Nothing outside the VPC can connect."
          actions={
            isAdmin ? (
              <Button
                variant="primary"
                disabled={!canDownloadConfig(cluster.state)}
                loading={isLoading}
                onClick={() => void save()}
                data-testid="download-spark-config"
              >
                Download client configuration
              </Button>
            ) : undefined
          }
        >
          Connect
        </Header>
      }
    >
      <SpaceBetween size="m">
        {isAdmin ? (
          <Box variant="p">
            The client configuration is a <Box variant="code">spark-defaults.conf</Box> with the
            master, the cluster&apos;s secret and, with object storage, its S3 settings: jobs need
            nothing else. It holds secrets, so keep it like a password.
          </Box>
        ) : (
          <Alert type="info">
            Submitting jobs needs the cluster&apos;s secret, which only an administrator can
            download.
          </Alert>
        )}
        {saved && <StatusIndicator type="success">Downloaded.</StatusIndicator>}
        {error !== undefined && (
          <Alert type="error" header="The client configuration could not be made">
            {errorMessage(error, 'The client configuration could not be made.')}
          </Alert>
        )}

        <ColumnLayout columns={2} variant="text-grid">
          <Row label="Master">
            {cluster.endpoint === null || cluster.endpoint === undefined ? (
              dash()
            ) : (
              <CopyToClipboard
                variant="inline"
                textToCopy={cluster.endpoint}
                copyButtonAriaLabel="Copy master URL"
                copySuccessText="Master URL copied"
                copyErrorText="Master URL not copied"
              />
            )}
          </Row>
          <Row label="spark-submit">
            <CopyToClipboard
              variant="inline"
              textToCopy={submitExample(cluster)}
              copyButtonAriaLabel="Copy spark-submit command"
              copySuccessText="Command copied"
              copyErrorText="Command not copied"
            />
          </Row>
          <Row label="Master web UI">{cluster.master_ui_url ?? dash()}</Row>
          <Row label="History server">
            {cluster.history_url ??
              (cluster.endpoint === null || cluster.endpoint === undefined
                ? dash()
                : 'Off: the cluster has no event-log bucket')}
          </Row>
        </ColumnLayout>

        <Alert type="info" header="Run spark-submit with the driver's address">
          The executors connect back to the driver, so the example passes{' '}
          <Box variant="code">spark.driver.host</Box> as the instance&apos;s address: Spark would
          otherwise advertise its hostname, which the workers may not resolve. The web UIs have no
          sign-in of their own; only the VPC reaches them.
        </Alert>
      </SpaceBetween>
    </Container>
  )
}
