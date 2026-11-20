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
import {
  clustersApi,
  pollInterval,
  useGetClusterQuery,
  useMintKubeconfigMutation,
  type Cluster,
  type ClusterNode,
  type NodeGroup,
} from '@/features/kubernetes/api'
import { DeleteClusterModal } from '@/features/kubernetes/DeleteClusterModal'
import { KubernetesGate } from '@/features/kubernetes/KubernetesGate'
import { ResizeNodeGroupModal } from '@/features/kubernetes/ResizeNodeGroupModal'
import {
  canDelete,
  canDownloadKubeconfig,
  canScale,
  CLUSTER_INDICATOR,
  kubectlExample,
  readyCount,
} from '@/features/kubernetes/state'
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

/** ClusterDetailPage is one cluster's page. */
export function ClusterDetailPage() {
  const { clusterId } = useParams<{ clusterId: string }>()
  const id = clusterId ?? ''
  usePageMeta({
    breadcrumbs: [
      { text: 'Kubernetes', href: '/kubernetes' },
      { text: 'Clusters', href: '/kubernetes' },
      { text: id, href: `/kubernetes/${id}` },
    ],
  })
  if (id === '') {
    return null
  }
  return (
    <KubernetesGate title={id}>
      <ClusterDetail id={id} />
    </KubernetesGate>
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

/** saveFile hands the browser a file to download, where it can. */
function saveFile(name: string, content: string) {
  if (typeof URL.createObjectURL !== 'function') {
    return
  }
  const url = URL.createObjectURL(new Blob([content], { type: 'application/yaml' }))
  const link = document.createElement('a')
  link.href = url
  link.download = name
  link.click()
  URL.revokeObjectURL(url)
}

function ClusterDetail({ id }: { id: string }) {
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const cached = useAppSelector(clustersApi.endpoints.getCluster.select(id))
  const taskArgs = useMemo(() => ({ resourceId: id }), [id])
  const cachedTasks = useAppSelector(tasksApi.endpoints.listTasks.select(taskArgs))
  const {
    data: cluster,
    isLoading,
    isFetching,
    error,
    refetch,
  } = useGetClusterQuery(id, {
    pollingInterval: pollInterval(cached.data === undefined ? undefined : [cached.data]),
  })
  const { data: tasksData } = useListTasksQuery(taskArgs, {
    pollingInterval: taskPollInterval(cachedTasks?.data?.items),
  })
  const [isDeleteOpen, setDeleteOpen] = useState(false)
  const [resizing, setResizing] = useState<NodeGroup | null>(null)

  if (isLoading) {
    return <Spinner size="large" />
  }
  if (error !== undefined && isNotFound(error)) {
    return (
      <ContentLayout header={<Header variant="h1">{id}</Header>}>
        <Alert
          type="info"
          header="This cluster is gone"
          action={<Button onClick={() => navigate('/kubernetes')}>Back to clusters</Button>}
        >
          It has been deleted, with its nodes and everything on them.
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

  const indicator = CLUSTER_INDICATOR[cluster.state]
  const { ready, total } = readyCount(cluster)

  return (
    <>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description={`Kubernetes ${cluster.kubernetes_version}, ${total} nodes`}
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
                  data-testid="delete-cluster"
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
                <span>{cluster.state_reason ?? 'The API server did not answer.'}</span>
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
              {cluster.state_reason ?? 'Something on the cluster is not healthy.'} Its control plane
              answers; it returns to active on its own once the cause is gone.
            </Alert>
          )}
          {cluster.state === 'creating' && (
            <Alert type="info" header="This cluster is being created">
              The control plane boots first, then the workers join it. The cluster is active once
              every node is Ready, usually within a few minutes.
            </Alert>
          )}

          <Tabs
            ariaLabel="Cluster"
            tabs={[
              {
                id: 'overview',
                label: 'Overview',
                content: (
                  <Overview
                    cluster={cluster}
                    ready={ready}
                    total={total}
                    indicator={indicator}
                    isAdmin={isAdmin}
                    onResize={setResizing}
                    tasks={tasksData?.items ?? []}
                  />
                ),
              },
              { id: 'nodes', label: `Nodes (${total})`, content: <Nodes nodes={cluster.nodes} /> },
              { id: 'connect', label: 'Connect', content: <Connect cluster={cluster} /> },
            ]}
          />
        </SpaceBetween>
      </ContentLayout>

      {isDeleteOpen && (
        <DeleteClusterModal
          cluster={cluster}
          onClose={() => setDeleteOpen(false)}
          onDeleted={() => navigate('/kubernetes')}
        />
      )}
      {resizing !== null && (
        <ResizeNodeGroupModal
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
  ready,
  total,
  indicator,
  isAdmin,
  onResize,
  tasks,
}: {
  cluster: Cluster
  ready: number
  total: number
  indicator: (typeof CLUSTER_INDICATOR)[keyof typeof CLUSTER_INDICATOR]
  isAdmin: boolean
  onResize: (group: NodeGroup) => void
  tasks: Task[]
}) {
  return (
    <SpaceBetween size="l">
      <Container header={<Header variant="h2">Details</Header>}>
        <ColumnLayout columns={4} variant="text-grid">
          <Row label="Status">
            <StatusIndicator type={indicator.type}>{indicator.label}</StatusIndicator>
          </Row>
          <Row label="Kubernetes">{cluster.kubernetes_version}</Row>
          <Row label="Nodes Ready">
            {ready} / {total}
          </Row>
          <Row label="Control plane size">{cluster.control_plane_type}</Row>
          <Row label="Pod range">
            <Box variant="code">{cluster.pod_cidr}</Box>
          </Row>
          <Row label="Service range">
            <Box variant="code">{cluster.service_cidr}</Box>
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
          { id: 'name', header: 'Name', cell: (g: NodeGroup) => g.name },
          { id: 'type', header: 'Size', cell: (g: NodeGroup) => g.instance_type },
          { id: 'disk', header: 'Root disk', cell: (g: NodeGroup) => `${g.root_disk_gb} GiB` },
          { id: 'desired', header: 'Nodes', cell: (g: NodeGroup) => g.desired_size },
          {
            id: 'actions',
            header: 'Actions',
            cell: (g: NodeGroup) => (
              <Button
                variant="inline-link"
                disabled={!isAdmin || !canScale(cluster.state)}
                onClick={() => onResize(g)}
                data-testid="resize-node-group"
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

function Nodes({ nodes }: { nodes: ClusterNode[] }) {
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
        { id: 'name', header: 'Name', isRowHeader: true, cell: (n: ClusterNode) => n.name },
        {
          id: 'role',
          header: 'Role',
          cell: (n: ClusterNode) => (n.role === 'control-plane' ? 'Control plane' : 'Worker'),
        },
        {
          id: 'ready',
          header: 'Kubernetes',
          cell: (n: ClusterNode) =>
            n.ready === null || n.ready === undefined ? (
              <StatusIndicator type="pending">Not yet seen</StatusIndicator>
            ) : n.ready ? (
              <StatusIndicator type="success">Ready</StatusIndicator>
            ) : (
              <StatusIndicator type="warning">Not Ready</StatusIndicator>
            ),
        },
        { id: 'state', header: 'VM', cell: (n: ClusterNode) => n.state },
        { id: 'ip', header: 'Address', cell: (n: ClusterNode) => n.private_ip },
        {
          id: 'instance',
          header: 'Instance',
          cell: (n: ClusterNode) => <Box variant="code">{n.instance_id}</Box>,
        },
      ]}
      ariaLabels={{ tableLabel: 'Nodes' }}
    />
  )
}

/**
 * Connect is the point of the page: a kubeconfig for the viewer, minted on
 * demand. A viewer gets one too -- in `kumo:viewers`, bound to `view` -- and
 * nothing is stored: each download is a new 30-day certificate
 * (08-kubernetes.md §5).
 */
function Connect({ cluster }: { cluster: Cluster }) {
  const dispatch = useAppDispatch()
  const isAdmin = useAppSelector(selectIsAdmin)
  const [mint, { isLoading }] = useMintKubeconfigMutation()
  const [expiresAt, setExpiresAt] = useState<string | null>(null)
  const [error, setError] = useState<ApiError>(undefined)

  async function download() {
    setError(undefined)
    try {
      const minted = await mint(cluster.id).unwrap()
      saveFile(`${cluster.name}.kubeconfig`, minted.kubeconfig)
      setExpiresAt(minted.expires_at)
      dispatch(
        notify({
          type: 'success',
          header: 'Kubeconfig downloaded',
          content: `${cluster.name}.kubeconfig, valid until ${formatAbsoluteTime(minted.expires_at)}.`,
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
            <Button
              variant="primary"
              disabled={!canDownloadKubeconfig(cluster.state)}
              loading={isLoading}
              onClick={() => void download()}
              data-testid="download-kubeconfig"
            >
              Download kubeconfig
            </Button>
          }
        >
          Connect
        </Header>
      }
    >
      <SpaceBetween size="m">
        <Box variant="p">
          The kubeconfig is signed for you, now, and stored nowhere. It is{' '}
          {isAdmin ? (
            <>
              an <strong>administrator&apos;s</strong>, in <Box variant="code">kumo:admins</Box>,
              bound to <Box variant="code">cluster-admin</Box>
            </>
          ) : (
            <>
              a <strong>viewer&apos;s</strong>, in <Box variant="code">kumo:viewers</Box>, bound to{' '}
              <Box variant="code">view</Box>
            </>
          )}
          , and valid for 30 days. Download another when it expires.
        </Box>
        {expiresAt !== null && (
          <StatusIndicator type="success">
            Downloaded; valid until {formatAbsoluteTime(expiresAt)}.
          </StatusIndicator>
        )}
        {error !== undefined && (
          <Alert type="error" header="The kubeconfig could not be made">
            {errorMessage(error, 'The kubeconfig could not be made.')}
          </Alert>
        )}

        <ColumnLayout columns={2} variant="text-grid">
          <Row label="Endpoint">
            {cluster.endpoint === null || cluster.endpoint === undefined ? (
              <Box variant="span" color="text-status-inactive">
                Not yet published
              </Box>
            ) : (
              <CopyToClipboard
                variant="inline"
                textToCopy={cluster.endpoint}
                copyButtonAriaLabel="Copy endpoint"
                copySuccessText="Endpoint copied"
                copyErrorText="Endpoint not copied"
              />
            )}
          </Row>
          <Row label="kubectl">
            <CopyToClipboard
              variant="inline"
              textToCopy={kubectlExample(cluster)}
              copyButtonAriaLabel="Copy kubectl command"
              copySuccessText="Command copied"
              copyErrorText="Command not copied"
            />
          </Row>
        </ColumnLayout>

        <Alert type="info" header="Pods do not get NET_RAW">
          The cluster runs CRI-O, whose default capabilities leave out{' '}
          <Box variant="code">NET_RAW</Box>: <Box variant="code">ping</Box> and other raw sockets
          fail inside a pod unless its <Box variant="code">securityContext</Box> adds the
          capability.
        </Alert>
      </SpaceBetween>
    </Container>
  )
}
