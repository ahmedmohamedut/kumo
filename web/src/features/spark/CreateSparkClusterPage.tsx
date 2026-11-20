import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Checkbox from '@cloudscape-design/components/checkbox'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Form from '@cloudscape-design/components/form'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import Multiselect, { type MultiselectProps } from '@cloudscape-design/components/multiselect'
import Select, { type SelectProps } from '@cloudscape-design/components/select'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import { useListBucketsQuery } from '@/features/buckets/api'
import { useListInstanceTypesQuery } from '@/features/instancetypes/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { useCreateSparkClusterMutation } from '@/features/spark/api'
import { NoSparkImageAlert, SparkGate, useSpark } from '@/features/spark/SparkGate'
import {
  bucketsProblem,
  MAX_NAME_LENGTH,
  MAX_WORKERS,
  MIN_MASTER_MEMORY_MIB,
  MIN_MASTER_VCPUS,
  MIN_WORKER_MEMORY_MIB,
  MIN_WORKERS,
  nameProblem,
  workerCountProblem,
} from '@/features/spark/state'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

/** The types the API defaults to. */
const DEFAULT_MASTER_TYPE = 'kumo.medium'
const DEFAULT_WORKER_TYPE = 'kumo.large'
const NO_EVENT_LOG: SelectProps.Option = {
  value: '',
  label: 'None: no event logs, no history server',
}

/** CreateSparkClusterPage is the Create Spark cluster form. */
export function CreateSparkClusterPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Spark', href: '/spark' },
      { text: 'Clusters', href: '/spark' },
      { text: 'Create cluster', href: '/spark/create' },
    ],
  })
  return (
    <SparkGate title="Create Spark cluster">
      <CreateSparkClusterForm />
    </SparkGate>
  )
}

/**
 * CreateSparkClusterForm creates a cluster: a master and one group of workers,
 * with object storage when the node has it. The size pickers only offer what a
 * node needs, and the bucket choices are checked as the API checks them, so
 * neither refusal has to come from the server.
 */
function CreateSparkClusterForm() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { versions, objectStorage: storageAvailable } = useSpark()
  const { data: types } = useListInstanceTypesQuery()
  const { data: buckets } = useListBucketsQuery(undefined, { skip: !storageAvailable })

  const [name, setName] = useState('')
  const [version, setVersion] = useState<SelectProps.Option | null>(null)
  const [masterType, setMasterType] = useState<SelectProps.Option | null>(null)
  const [workerType, setWorkerType] = useState<SelectProps.Option | null>(null)
  const [workerCount, setWorkerCount] = useState('2')
  const [objectStorage, setObjectStorage] = useState(false)
  const [chosen, setChosen] = useState<ReadonlyArray<MultiselectProps.Option>>([])
  const [eventLog, setEventLog] = useState<SelectProps.Option>(NO_EVENT_LOG)
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})
  const [createCluster, { isLoading }] = useCreateSparkClusterMutation()

  const errors = { ...fieldErrors(error), ...localErrors }

  const versionOptions: SelectProps.Options = versions.map((v) => ({
    value: v,
    label: `Spark ${v}`,
  }))
  const toOption = (type: { id: string; vcpus: number; memory_mib: number }) => ({
    value: type.id,
    label: type.id,
    description: `${type.vcpus} vCPU, ${type.memory_mib} MiB memory`,
  })
  const masterOptions: SelectProps.Options = (types ?? [])
    .filter((t) => t.vcpus >= MIN_MASTER_VCPUS && t.memory_mib >= MIN_MASTER_MEMORY_MIB)
    .map(toOption)
  const workerOptions: SelectProps.Options = (types ?? [])
    .filter((t) => t.memory_mib >= MIN_WORKER_MEMORY_MIB)
    .map(toOption)
  const byDefault = (options: SelectProps.Options, id: string) =>
    (options.find((o) => 'value' in o && o.value === id) ??
      options[0] ??
      null) as SelectProps.Option | null

  const versionChoice = version ?? (versionOptions[0] as SelectProps.Option | undefined) ?? null
  const masterChoice = masterType ?? byDefault(masterOptions, DEFAULT_MASTER_TYPE)
  const workerChoice = workerType ?? byDefault(workerOptions, DEFAULT_WORKER_TYPE)
  const bucketOptions: MultiselectProps.Options = (buckets ?? []).map((b) => ({
    value: b.name,
    label: b.name,
  }))
  const chosenNames = chosen.map((o) => o.value ?? '')
  const eventLogOptions: SelectProps.Options = [
    NO_EVENT_LOG,
    ...chosenNames.map((b) => ({ value: b, label: b })),
  ]
  const eventLogChoice = chosenNames.includes(eventLog.value ?? '') ? eventLog : NO_EVENT_LOG

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError(undefined)
    const invalid: Record<string, string> = {}
    const nameIssue = nameProblem(name.trim())
    if (nameIssue !== null) {
      invalid['name'] = nameIssue
    }
    const countIssue = workerCountProblem(Number(workerCount))
    if (countIssue !== null) {
      invalid['worker_count'] = countIssue
    }
    if (masterChoice?.value === undefined) {
      invalid['master_type'] = 'Choose a size for the master.'
    }
    if (workerChoice?.value === undefined) {
      invalid['worker_type'] = 'Choose a size for the workers.'
    }
    if (objectStorage) {
      const bucketIssue = bucketsProblem(chosenNames, eventLogChoice.value ?? '')
      if (bucketIssue !== null) {
        invalid['buckets'] = bucketIssue
      }
    }
    setLocalErrors(invalid)
    if (Object.keys(invalid).length > 0) {
      return
    }

    try {
      const created = await createCluster({
        name: name.trim(),
        ...(versionChoice?.value === undefined ? {} : { spark_version: versionChoice.value }),
        master_type: String(masterChoice?.value),
        worker_type: String(workerChoice?.value),
        worker_count: Number(workerCount),
        object_storage: objectStorage,
        ...(objectStorage
          ? {
              buckets: chosenNames,
              ...(eventLogChoice.value === '' ? {} : { event_log_bucket: eventLogChoice.value }),
            }
          : {}),
      }).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'Spark cluster creation started',
          content: `${created.name} is being created. It is ready once every worker has registered with the master.`,
        }),
      )
      navigate(`/spark/${created.id}`)
    } catch (err) {
      setError(err as ApiError)
    }
  }

  if (versions.length === 0) {
    return (
      <ContentLayout header={<Header variant="h1">Create Spark cluster</Header>}>
        <NoSparkImageAlert />
      </ContentLayout>
    )
  }

  return (
    <form onSubmit={submit}>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description="A Spark master and a group of workers, on virtual machines Kumo runs for you."
          >
            Create Spark cluster
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/spark')} formAction="none">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isLoading}
                loadingText="Creating"
                disabled={!isAdmin}
                data-testid="submit-create-spark-cluster"
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
                  description="Unique on this node. The master is <name>.<vpc> in the VPC's DNS."
                  constraintText={`1-${MAX_NAME_LENGTH} characters: a letter, then lowercase letters, digits and single dashes.`}
                  errorText={errors['name']}
                >
                  <Input
                    value={name}
                    onChange={({ detail }) => setName(detail.value)}
                    ariaLabel="Cluster name"
                    placeholder="etl"
                    autoFocus
                  />
                </FormField>
                <FormField
                  label="Spark version"
                  description="The versions this node has an engine image for."
                >
                  <Select
                    selectedOption={versionChoice}
                    options={versionOptions}
                    onChange={({ detail }) => setVersion(detail.selectedOption)}
                    ariaLabel="Spark version"
                  />
                </FormField>
                <FormField
                  label="Master size"
                  description={`At least ${MIN_MASTER_VCPUS} vCPU and ${MIN_MASTER_MEMORY_MIB} MiB: the master and the history server.`}
                  errorText={errors['master_type']}
                >
                  <Select
                    selectedOption={masterChoice}
                    options={masterOptions}
                    onChange={({ detail }) => setMasterType(detail.selectedOption)}
                    ariaLabel="Master size"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h2">Workers</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Worker size"
                  description={`At least ${MIN_WORKER_MEMORY_MIB} MiB. A worker offers executors its vCPUs and its memory less 1 GiB.`}
                  errorText={errors['worker_type']}
                >
                  <Select
                    selectedOption={workerChoice}
                    options={workerOptions}
                    onChange={({ detail }) => setWorkerType(detail.selectedOption)}
                    ariaLabel="Worker size"
                  />
                </FormField>
                <FormField
                  label="Workers"
                  description="The cluster's one node group. It can be resized later."
                  constraintText={`${MIN_WORKERS}-${MAX_WORKERS}.`}
                  errorText={errors['worker_count']}
                >
                  <Input
                    type="number"
                    value={workerCount}
                    onChange={({ detail }) => setWorkerCount(detail.value)}
                    ariaLabel="Workers"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Container
              header={
                <Header
                  variant="h2"
                  description="Jobs read and write s3a:// with nothing to configure, through an access key of the cluster's own."
                >
                  Object storage
                </Header>
              }
            >
              {storageAvailable ? (
                <SpaceBetween size="m">
                  <Checkbox
                    checked={objectStorage}
                    onChange={({ detail }) => setObjectStorage(detail.checked)}
                    data-testid="spark-object-storage"
                  >
                    Give the cluster access to buckets
                  </Checkbox>
                  {objectStorage && (
                    <>
                      <FormField
                        label="Buckets"
                        description="The cluster's key may read and write these, and nothing else. They are kept when the cluster is deleted."
                        errorText={errors['buckets'] ?? errors['event_log_bucket']}
                      >
                        <Multiselect
                          selectedOptions={chosen}
                          options={bucketOptions}
                          onChange={({ detail }) => setChosen(detail.selectedOptions)}
                          placeholder="Choose buckets"
                          empty="No buckets yet: create one on the Storage pages first."
                          ariaLabel="Buckets"
                        />
                      </FormField>
                      <FormField
                        label="Event logs"
                        description="One of the buckets keeps every job's event log, which the history server reads. Without one there is no history server."
                      >
                        <Select
                          selectedOption={eventLogChoice}
                          options={eventLogOptions}
                          onChange={({ detail }) => setEventLog(detail.selectedOption)}
                          ariaLabel="Event-log bucket"
                        />
                      </FormField>
                    </>
                  )}
                </SpaceBetween>
              ) : (
                <Box variant="p" color="text-status-inactive">
                  Object storage is off on this node, so the cluster runs without it: no buckets, no
                  event logs and no history server.
                </Box>
              )}
            </Container>

            <Alert type="info" header="Reachable from inside its VPC">
              Jobs are submitted with <Box variant="code">spark-submit</Box> from an instance in the
              same VPC, with the client configuration the cluster&apos;s Connect tab downloads. The
              web UIs have no sign-in of their own and are not reachable from outside the VPC.
            </Alert>
          </SpaceBetween>
        </Form>
      </ContentLayout>
    </form>
  )
}
