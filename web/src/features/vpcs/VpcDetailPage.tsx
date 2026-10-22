import { useMemo, useState, type ReactNode } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Badge from '@cloudscape-design/components/badge'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import ColumnLayout from '@cloudscape-design/components/column-layout'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Link from '@cloudscape-design/components/link'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import StatusIndicator from '@cloudscape-design/components/status-indicator'
import TagEditor, { type TagEditorProps } from '@cloudscape-design/components/tag-editor'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { useDeleteVPCMutation, useGetVPCQuery, useUpdateVPCMutation } from '@/features/vpcs/api'
import { useListSubnetsQuery } from '@/features/subnets/api'
import {
  pollInterval as taskPollInterval,
  tasksApi,
  useListTasksQuery,
  type Task,
} from '@/features/tasks/api'
import { taskIndicatorType, taskLabel } from '@/features/tasks/state'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/**
 * VpcDetailPage shows one VPC: the address block, the network that carries it,
 * the subnets partitioned out of it and the tasks that worked on it.
 *
 * The counters come from the detail endpoint (`subnet_count`,
 * `instance_count`) rather than being counted locally, because the local list
 * may be filtered or paged — the API knows how many rows really live inside.
 *
 * Creating and deleting a VPC are tasks (the network is defined and started in
 * the background), so the activity list polls at 5 s while one of those is in
 * flight and 30 s once the queue is quiet (03-frontend.md §5).
 */
export function VpcDetailPage() {
  const { vpcId } = useParams<{ vpcId: string }>()
  const dispatch = useAppDispatch()
  const navigate = useNavigate()

  const id = vpcId ?? ''
  // Memoized so the subscription keys stay stable across renders.
  const subnetArgs = useMemo(() => ({ vpcId: id }), [id])
  const taskArgs = useMemo(() => ({ resourceId: id }), [id])
  const cachedTasks = useAppSelector(tasksApi.endpoints.listTasks.select(taskArgs))

  const {
    data: vpc,
    isLoading,
    isFetching,
    error,
    refetch,
  } = useGetVPCQuery(id, { skip: id === '' })
  const { data: subnetsData } = useListSubnetsQuery(subnetArgs, { skip: id === '' })
  const { data: tasksData } = useListTasksQuery(taskArgs, {
    skip: id === '',
    pollingInterval: taskPollInterval(cachedTasks?.data?.items),
  })

  const [deleteVPC, { isLoading: isDeleting }] = useDeleteVPCMutation()
  const [updateVPC, { isLoading: isSavingTags }] = useUpdateVPCMutation()

  const [isDeleteOpen, setDeleteOpen] = useState(false)
  const [actionError, setActionError] = useState<ApiError>(undefined)
  const [isEditing, setIsEditing] = useState(false)
  const [editTags, setEditTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])

  usePageMeta({
    breadcrumbs: [
      { text: 'Networking', href: '/' },
      { text: 'VPCs', href: '/vpcs' },
      { text: vpc?.name ?? id, href: `/vpcs/${id}` },
    ],
  })

  const subnets = subnetsData?.items ?? []
  const tasks = tasksData?.items ?? []
  const tags = Object.entries(vpc?.tags ?? {})

  /** beginEditing seeds the tag editor from what the VPC carries now. */
  function beginEditing() {
    setEditTags(tags.map(([key, value]) => ({ key, value, existing: true })))
    setIsEditing(true)
  }

  async function saveTags() {
    if (vpc === undefined) {
      return
    }
    setActionError(undefined)
    try {
      await updateVPC({
        id: vpc.id,
        tags: Object.fromEntries(editTags.map((tag) => [tag.key, tag.value])),
      }).unwrap()
      setIsEditing(false)
      dispatch(notify({ type: 'success', header: 'Tags updated', content: vpc.name }))
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  async function onDelete() {
    if (vpc === undefined) {
      return
    }
    setActionError(undefined)
    try {
      await deleteVPC(vpc.id).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'VPC deletion started',
          content: 'The network is destroyed and the row removed by a background task.',
        }),
      )
      navigate('/vpcs')
    } catch (err) {
      // A 409 names the subnets that still live inside, so stay on the page and
      // show it: navigating away would throw that answer away.
      setActionError(err as ApiError)
      setDeleteOpen(false)
    }
  }

  if (id === '') {
    return null
  }

  return (
    <ContentLayout
      header={
        <Header
          variant="h1"
          description={vpc === undefined ? undefined : <Box variant="code">{vpc.id}</Box>}
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button
                iconName="refresh"
                ariaLabel="Refresh VPC"
                loading={isFetching}
                onClick={() => void refetch()}
              />
              <Button onClick={() => setDeleteOpen(true)} data-testid="delete-vpc">
                Delete
              </Button>
              <Button onClick={() => navigate('/vpcs')} data-testid="back-to-vpcs">
                Back to VPCs
              </Button>
            </SpaceBetween>
          }
        >
          {vpc?.name ?? 'VPC'}
        </Header>
      }
    >
      <SpaceBetween size="l">
        {error !== undefined && (
          <Alert type="error" header="The VPC could not be loaded">
            {errorMessage(error, 'The VPC could not be loaded.')}
          </Alert>
        )}

        {actionError !== undefined && (
          <Alert type="error" header="The request was refused">
            {errorMessage(actionError, 'The request could not be carried out.')}
          </Alert>
        )}

        {isLoading && (
          <Box textAlign="center" padding="l">
            <Spinner size="large" />
          </Box>
        )}

        {vpc !== undefined && (
          <Container header={<Header variant="h2">Details</Header>}>
            <ColumnLayout columns={2} variant="text-grid">
              <Row label="VPC ID">
                <Box variant="code">{vpc.id}</Box>
              </Row>
              <Row label="Default VPC">
                {vpc.is_default ? <Badge color="green">Default</Badge> : '—'}
              </Row>
              <Row label="IPv4 CIDR">
                <Box variant="code">{vpc.cidr}</Box>
              </Row>
              <Row label="IPv4 gateway">
                <Box variant="code">{vpc.gateway_ip}</Box>
              </Row>
              <Row label="Netmask">
                <Box variant="code">{vpc.netmask}</Box>
              </Row>
              <Row label="Connectivity">{vpc.mode === 'nat' ? 'Internet (NAT)' : 'Isolated'}</Row>
              <Row label="Bridge interface">
                <Box variant="code">{vpc.bridge_name}</Box>
              </Row>
              <Row label="Libvirt network">{vpc.libvirt_network}</Row>
              <Row label="Subnets">{vpc.subnet_count}</Row>
              <Row label="Instances">{vpc.instance_count}</Row>
              <Row label="Created">
                <Time value={vpc.created_at} />
              </Row>
              <Row label="Last updated">
                <Time value={vpc.updated_at} />
              </Row>
            </ColumnLayout>
          </Container>
        )}

        {vpc !== undefined && (
          <Container
            header={
              <Header
                variant="h2"
                description="The address partitions instances attached to this VPC live in."
                actions={
                  <Button onClick={() => navigate('/subnets/create')} data-testid="new-subnet">
                    Create subnet
                  </Button>
                }
              >
                Subnets
              </Header>
            }
          >
            {subnets.length === 0 ? (
              <Box variant="p" color="text-status-inactive">
                No subnets have been carved out of this VPC yet.
              </Box>
            ) : (
              <SpaceBetween size="s">
                {subnets.map((subnet) => (
                  <SpaceBetween key={subnet.id} direction="horizontal" size="s">
                    <Link href={`/subnets/${subnet.id}`}>{subnet.name}</Link>
                    {subnet.is_default && <Badge color="green">Default</Badge>}
                    <Box variant="code">{subnet.cidr}</Box>
                    <Box variant="small" color="text-status-inactive">
                      {subnet.usable_ips - subnet.allocated_ips} IPs available
                    </Box>
                  </SpaceBetween>
                ))}
              </SpaceBetween>
            )}
          </Container>
        )}

        {vpc !== undefined && (
          <Container
            header={
              <Header
                variant="h2"
                description="Key/value pairs to organize VPCs. The address block and mode are immutable."
                actions={
                  isEditing ? (
                    <SpaceBetween direction="horizontal" size="s">
                      <Button variant="link" onClick={() => setIsEditing(false)}>
                        Cancel
                      </Button>
                      <Button
                        variant="primary"
                        loading={isSavingTags}
                        onClick={() => void saveTags()}
                      >
                        Save
                      </Button>
                    </SpaceBetween>
                  ) : (
                    <Button onClick={beginEditing}>Edit tags</Button>
                  )
                }
              >
                Tags
              </Header>
            }
          >
            {isEditing ? (
              <FormField
                label="Tags"
                errorText={fieldErrors(actionError)['tags']}
                description="Optional key/value pairs to organize VPCs."
              >
                <TagEditor
                  tags={editTags}
                  onChange={({ detail }) => setEditTags(detail.tags)}
                  i18nStrings={{ keyPlaceholder: 'Enter a tag key' }}
                  tagLimit={50}
                />
              </FormField>
            ) : tags.length === 0 ? (
              <Box variant="p" color="text-status-inactive">
                This VPC has no tags.
              </Box>
            ) : (
              <ColumnLayout columns={2} variant="text-grid">
                {tags.map(([key, value]) => (
                  <Row key={key} label={key}>
                    {value === '' ? '—' : value}
                  </Row>
                ))}
              </ColumnLayout>
            )}
          </Container>
        )}

        {vpc !== undefined && (
          <Container header={<Header variant="h2">Activity</Header>}>
            <SpaceBetween size="s">
              {tasks.length === 0 ? (
                <Box variant="p" color="text-status-inactive">
                  No task has touched this VPC.
                </Box>
              ) : (
                tasks.map((task) => <TaskRow key={task.id} task={task} />)
              )}
            </SpaceBetween>
          </Container>
        )}
      </SpaceBetween>

      {isDeleteOpen && vpc !== undefined && (
        <Modal
          visible
          onDismiss={() => setDeleteOpen(false)}
          header={`Delete ${vpc.name}?`}
          closeAriaLabel="Close"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="s">
                <Button variant="link" onClick={() => setDeleteOpen(false)}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  loading={isDeleting}
                  onClick={() => void onDelete()}
                  data-testid="confirm-delete-vpc"
                >
                  Delete
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Alert type="warning" header="This cannot be undone">
              Deleting the VPC destroys its libvirt network and removes the row.
            </Alert>
            <Box variant="p">
              A VPC that still contains subnets or instances cannot be deleted; delete the subnets
              and terminate the instances first.
            </Box>
          </SpaceBetween>
        </Modal>
      )}
    </ContentLayout>
  )
}

/** Row is one label/value line of a details container: the label in muted text,
 *  the value below it. */
function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <SpaceBetween size="xxs">
      <Box variant="awsui-key-label">{label}</Box>
      <Box>{children}</Box>
    </SpaceBetween>
  )
}

/** Time renders a timestamp as relative text with the absolute time as its title. */
function Time({ value }: { value: string }) {
  return <span title={formatAbsoluteTime(value)}>{formatRelativeTime(value)}</span>
}

/** TaskRow is one task in the activity list: what it was and how it ended. */
function TaskRow({ task }: { task: Task }) {
  return (
    <SpaceBetween size="xs">
      <SpaceBetween direction="horizontal" size="s">
        <Box variant="code">{task.type}</Box>
        <StatusIndicator type={taskIndicatorType(task.state)}>
          {taskLabel(task.state)}
        </StatusIndicator>
      </SpaceBetween>
      <Box variant="small" color="text-status-inactive">
        {task.error ?? `queued ${formatRelativeTime(task.created_at)}`}
      </Box>
    </SpaceBetween>
  )
}
