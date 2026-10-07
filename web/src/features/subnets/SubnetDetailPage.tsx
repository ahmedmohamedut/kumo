import { useState, type ReactNode } from 'react'
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
import TagEditor, { type TagEditorProps } from '@cloudscape-design/components/tag-editor'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch } from '@/app/store'
import {
  useDeleteSubnetMutation,
  useGetSubnetQuery,
  useUpdateSubnetMutation,
} from '@/features/subnets/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'
import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/**
 * SubnetDetailPage shows one subnet: the slice of its VPC's address block it
 * owns, how much of it is left, and its tags.
 *
 * A subnet is a row, not a libvirt object, so creating and deleting one are
 * synchronous (`201`/`204`): there is no task list here, and nothing to poll.
 * The only thing this page can do wrong is delete a subnet an interface still
 * holds an address from, and the API's `409` names that condition.
 */
export function SubnetDetailPage() {
  const { subnetId } = useParams<{ subnetId: string }>()
  const dispatch = useAppDispatch()
  const navigate = useNavigate()

  const id = subnetId ?? ''
  const {
    data: subnet,
    isLoading,
    isFetching,
    error,
    refetch,
  } = useGetSubnetQuery(id, { skip: id === '' })

  const [deleteSubnet, { isLoading: isDeleting }] = useDeleteSubnetMutation()
  const [updateSubnet, { isLoading: isSavingTags }] = useUpdateSubnetMutation()

  const [isDeleteOpen, setDeleteOpen] = useState(false)
  const [actionError, setActionError] = useState<ApiError>(undefined)
  const [isEditing, setIsEditing] = useState(false)
  const [editTags, setEditTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])

  usePageMeta({
    breadcrumbs: [
      { text: 'Networking', href: '/' },
      { text: 'Subnets', href: '/subnets' },
      { text: subnet?.name ?? id, href: `/subnets/${id}` },
    ],
  })

  const tags = Object.entries(subnet?.tags ?? {})

  /** beginEditing seeds the tag editor from what the subnet carries now. */
  function beginEditing() {
    setEditTags(tags.map(([key, value]) => ({ key, value, existing: true })))
    setIsEditing(true)
  }

  async function saveTags() {
    if (subnet === undefined) {
      return
    }
    setActionError(undefined)
    try {
      await updateSubnet({
        id: subnet.id,
        tags: Object.fromEntries(editTags.map((tag) => [tag.key, tag.value])),
      }).unwrap()
      setIsEditing(false)
      dispatch(notify({ type: 'success', header: 'Tags updated', content: subnet.name }))
    } catch (err) {
      setActionError(err as ApiError)
    }
  }

  async function onDelete() {
    if (subnet === undefined) {
      return
    }
    setActionError(undefined)
    try {
      await deleteSubnet(subnet.id).unwrap()
      dispatch(
        notify({
          type: 'success',
          header: 'Subnet deleted',
          content: `${subnet.name}: the address block is free again.`,
        }),
      )
      navigate('/subnets')
    } catch (err) {
      // A 409 names the condition — an address still allocated — so stay on the
      // page and show it: navigating away would throw that answer away.
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
          description={subnet === undefined ? undefined : <Box variant="code">{subnet.id}</Box>}
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button
                iconName="refresh"
                ariaLabel="Refresh subnet"
                loading={isFetching}
                onClick={() => void refetch()}
              />
              <Button onClick={() => setDeleteOpen(true)} data-testid="delete-subnet">
                Delete
              </Button>
              <Button onClick={() => navigate('/subnets')} data-testid="back-to-subnets">
                Back to subnets
              </Button>
            </SpaceBetween>
          }
        >
          {subnet?.name ?? 'Subnet'}
        </Header>
      }
    >
      <SpaceBetween size="l">
        {error !== undefined && (
          <Alert type="error" header="The subnet could not be loaded">
            {errorMessage(error, 'The subnet could not be loaded.')}
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

        {subnet !== undefined && (
          <Container header={<Header variant="h2">Details</Header>}>
            <ColumnLayout columns={2} variant="text-grid">
              <Row label="Subnet ID">
                <Box variant="code">{subnet.id}</Box>
              </Row>
              <Row label="Default subnet">
                {subnet.is_default ? <Badge color="green">Default</Badge> : '—'}
              </Row>
              <Row label="Connected VPC">
                <Link href={`/vpcs/${subnet.vpc_id}`}>{subnet.vpc_id}</Link>
              </Row>
              <Row label="IPv4 CIDR">
                <Box variant="code">{subnet.cidr}</Box>
              </Row>
              <Row label="Total IPs">{subnet.total_ips.toLocaleString('en-US')}</Row>
              <Row label="Usable IPs">{subnet.usable_ips.toLocaleString('en-US')}</Row>
              <Row label="Allocated IPs">{subnet.allocated_ips.toLocaleString('en-US')}</Row>
              <Row label="IPs available">
                {(subnet.usable_ips - subnet.allocated_ips).toLocaleString('en-US')}
              </Row>
              <Row label="Created">
                <Time value={subnet.created_at} />
              </Row>
              <Row label="Last updated">
                <Time value={subnet.updated_at} />
              </Row>
            </ColumnLayout>
          </Container>
        )}

        {subnet !== undefined && (
          <Container
            header={
              <Header
                variant="h2"
                description="Key/value pairs to organize subnets. The VPC, name and CIDR are immutable."
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
                description="Optional key/value pairs to organize subnets."
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
                This subnet has no tags.
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
      </SpaceBetween>

      {isDeleteOpen && subnet !== undefined && (
        <Modal
          visible
          onDismiss={() => setDeleteOpen(false)}
          header={`Delete ${subnet.name}?`}
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
                  data-testid="confirm-delete-subnet"
                >
                  Delete
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <SpaceBetween size="m">
            <Alert type="warning" header="This cannot be undone">
              Deleting the subnet frees its address block back to the VPC.
            </Alert>
            <Box variant="p">
              A subnet an instance still holds an address from cannot be deleted; terminate the
              instances first.
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
