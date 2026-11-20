import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Form from '@cloudscape-design/components/form'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import Select, { type SelectProps } from '@cloudscape-design/components/select'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import { useListInstanceTypesQuery } from '@/features/instancetypes/api'
import { useCreateClusterMutation } from '@/features/kubernetes/api'
import {
  KubernetesGate,
  NoKubernetesImageAlert,
  useKubernetesVersions,
} from '@/features/kubernetes/KubernetesGate'
import {
  MAX_NAME_LENGTH,
  MAX_NODE_COUNT,
  MIN_CONTROL_PLANE_VCPUS,
  MIN_NODE_COUNT,
  MIN_NODE_MEMORY_MIB,
  nameProblem,
  nodeCountProblem,
} from '@/features/kubernetes/state'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

/** The type a cluster's nodes default to, as the API's does. */
const DEFAULT_TYPE = 'kumo.medium'

/** CreateClusterPage is the Create cluster form. */
export function CreateClusterPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Kubernetes', href: '/kubernetes' },
      { text: 'Clusters', href: '/kubernetes' },
      { text: 'Create cluster', href: '/kubernetes/create' },
    ],
  })
  return (
    <KubernetesGate title="Create cluster">
      <CreateClusterForm />
    </KubernetesGate>
  )
}

/**
 * CreateClusterForm creates a cluster: a control plane and one group of
 * workers. The type pickers only offer what kubeadm accepts -- 2 vCPU for the
 * control plane, 2048 MiB for every node -- so the refusal never has to come
 * from the server.
 */
function CreateClusterForm() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { versions } = useKubernetesVersions()
  const { data: types } = useListInstanceTypesQuery()

  const [name, setName] = useState('')
  const [version, setVersion] = useState<SelectProps.Option | null>(null)
  const [controlPlaneType, setControlPlaneType] = useState<SelectProps.Option | null>(null)
  const [nodeType, setNodeType] = useState<SelectProps.Option | null>(null)
  const [nodeCount, setNodeCount] = useState('2')
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})
  const [createCluster, { isLoading }] = useCreateClusterMutation()

  const errors = { ...fieldErrors(error), ...localErrors }

  const versionOptions: SelectProps.Options = versions.map((v) => ({
    value: v,
    label: `Kubernetes ${v}`,
  }))
  const toOption = (type: { id: string; vcpus: number; memory_mib: number }) => ({
    value: type.id,
    label: type.id,
    description: `${type.vcpus} vCPU, ${type.memory_mib} MiB memory`,
  })
  const nodeTypes = (types ?? []).filter((type) => type.memory_mib >= MIN_NODE_MEMORY_MIB)
  const controlPlaneTypes = nodeTypes.filter((type) => type.vcpus >= MIN_CONTROL_PLANE_VCPUS)
  const controlPlaneOptions: SelectProps.Options = controlPlaneTypes.map(toOption)
  const nodeOptions: SelectProps.Options = nodeTypes.map(toOption)
  const byDefault = (options: SelectProps.Options) =>
    (options.find((o) => 'value' in o && o.value === DEFAULT_TYPE) ??
      options[0] ??
      null) as SelectProps.Option | null

  const versionChoice = version ?? (versionOptions[0] as SelectProps.Option | undefined) ?? null
  const controlPlaneChoice = controlPlaneType ?? byDefault(controlPlaneOptions)
  const nodeChoice = nodeType ?? byDefault(nodeOptions)

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError(undefined)
    const invalid: Record<string, string> = {}
    const nameIssue = nameProblem(name.trim())
    if (nameIssue !== null) {
      invalid['name'] = nameIssue
    }
    const countIssue = nodeCountProblem(Number(nodeCount))
    if (countIssue !== null) {
      invalid['node_count'] = countIssue
    }
    if (controlPlaneChoice?.value === undefined) {
      invalid['control_plane_type'] = 'Choose a size for the control plane.'
    }
    if (nodeChoice?.value === undefined) {
      invalid['node_type'] = 'Choose a size for the workers.'
    }
    setLocalErrors(invalid)
    if (Object.keys(invalid).length > 0) {
      return
    }

    try {
      const created = await createCluster({
        name: name.trim(),
        ...(versionChoice?.value === undefined ? {} : { kubernetes_version: versionChoice.value }),
        control_plane_type: String(controlPlaneChoice?.value),
        node_type: String(nodeChoice?.value),
        node_count: Number(nodeCount),
      }).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'Cluster creation started',
          content: `${created.name} is being created. It is ready once every node is Ready, usually within a few minutes.`,
        }),
      )
      navigate(`/kubernetes/${created.id}`)
    } catch (err) {
      setError(err as ApiError)
    }
  }

  if (versions.length === 0) {
    return (
      <ContentLayout header={<Header variant="h1">Create cluster</Header>}>
        <NoKubernetesImageAlert />
      </ContentLayout>
    )
  }

  return (
    <form onSubmit={submit}>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description="A Kubernetes control plane and a group of workers, on virtual machines Kumo runs for you."
          >
            Create cluster
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/kubernetes')} formAction="none">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isLoading}
                loadingText="Creating"
                disabled={!isAdmin}
                data-testid="submit-create-cluster"
              >
                Create cluster
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {!isAdmin && <Alert type="info">Only an administrator can create clusters.</Alert>}
            {error !== undefined && Object.keys(fieldErrors(error)).length === 0 && (
              <Alert type="error" header="The cluster was not created">
                {errorMessage(error, 'The cluster could not be created.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">Cluster</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Name"
                  description="Unique on this node. The API endpoint is api.<name>.<vpc>, and the nodes are named after it."
                  constraintText={`1-${MAX_NAME_LENGTH} characters: a letter, then lowercase letters, digits and single dashes.`}
                  errorText={errors['name']}
                >
                  <Input
                    value={name}
                    onChange={({ detail }) => setName(detail.value)}
                    ariaLabel="Cluster name"
                    placeholder="orders"
                    autoFocus
                  />
                </FormField>
                <FormField
                  label="Kubernetes version"
                  description="The versions this node has an engine image for."
                >
                  <Select
                    selectedOption={versionChoice}
                    options={versionOptions}
                    onChange={({ detail }) => setVersion(detail.selectedOption)}
                    ariaLabel="Kubernetes version"
                  />
                </FormField>
                <FormField
                  label="Control plane size"
                  description={`At least ${MIN_CONTROL_PLANE_VCPUS} vCPU and ${MIN_NODE_MEMORY_MIB} MiB, kubeadm's minimum.`}
                  errorText={errors['control_plane_type']}
                >
                  <Select
                    selectedOption={controlPlaneChoice}
                    options={controlPlaneOptions}
                    onChange={({ detail }) => setControlPlaneType(detail.selectedOption)}
                    ariaLabel="Control plane size"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h2">Workers</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Worker size"
                  description={`At least ${MIN_NODE_MEMORY_MIB} MiB.`}
                  errorText={errors['node_type']}
                >
                  <Select
                    selectedOption={nodeChoice}
                    options={nodeOptions}
                    onChange={({ detail }) => setNodeType(detail.selectedOption)}
                    ariaLabel="Worker size"
                  />
                </FormField>
                <FormField
                  label="Workers"
                  description="The cluster's one node group. It can be resized later."
                  constraintText={`${MIN_NODE_COUNT}-${MAX_NODE_COUNT}.`}
                  errorText={errors['node_count']}
                >
                  <Input
                    type="number"
                    value={nodeCount}
                    onChange={({ detail }) => setNodeCount(detail.value)}
                    ariaLabel="Workers"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Alert type="info" header="Reachable from inside its VPC">
              The API server and every node are on the VPC only, with no public endpoint: use
              <Box variant="code">kubectl</Box> from an instance in the same VPC. Volumes use the
              default <Box variant="code">local-path</Box> storage class, on the nodes&apos; own
              disks.
            </Alert>
          </SpaceBetween>
        </Form>
      </ContentLayout>
    </form>
  )
}
