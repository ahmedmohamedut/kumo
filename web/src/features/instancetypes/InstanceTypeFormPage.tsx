import { useState, type FormEvent } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Button from '@cloudscape-design/components/button'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Form from '@cloudscape-design/components/form'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import {
  useCreateInstanceTypeMutation,
  useGetInstanceTypeQuery,
  useUpdateInstanceTypeMutation,
  type InstanceType,
} from '@/features/instancetypes/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, isNotFound, type ApiError } from '@/lib/apiError'
import { formatMemoryMiB } from '@/lib/format'

/**
 * The bounds of 01-api.md §Instance types, checked here before a request is
 * spent; the API checks them again and stays the authority.
 */
const ID_PATTERN = /^[a-z0-9][a-z0-9._-]{0,63}$/
const MIN_VCPUS = 1
const MAX_VCPUS = 256
const MIN_MEMORY_MIB = 256
const MAX_MEMORY_MIB = 1024 * 1024
const MAX_DESCRIPTION = 255

/** CreateInstanceTypePage is `/instance-types/create`. */
export function CreateInstanceTypePage() {
  return <InstanceTypeForm mode="create" />
}

/**
 * EditInstanceTypePage is `/instance-types/:instanceTypeId/edit`. It loads the
 * type first and only then mounts the form, so the inputs start from the
 * stored values instead of being patched in by an effect after a blank render.
 */
export function EditInstanceTypePage() {
  const { instanceTypeId = '' } = useParams()
  const { data, isLoading, error } = useGetInstanceTypeQuery(instanceTypeId)

  if (isLoading) {
    return <Spinner size="large" />
  }
  if (data === undefined) {
    return (
      <Alert
        type={isNotFound(error) ? 'info' : 'error'}
        header={
          isNotFound(error)
            ? 'This instance type no longer exists'
            : 'The instance type could not be loaded'
        }
      >
        {errorMessage(error, `The instance type ${instanceTypeId} could not be loaded.`)}
      </Alert>
    )
  }
  return <InstanceTypeForm mode="edit" initial={data} />
}

type InstanceTypeFormProps = { mode: 'create' } | { mode: 'edit'; initial: InstanceType }

/** validate returns the local field errors of the form, empty when it may be sent. */
function validate(
  mode: 'create' | 'edit',
  id: string,
  vcpus: string,
  memoryMiB: string,
  description: string,
): Record<string, string> {
  const errors: Record<string, string> = {}
  if (mode === 'create' && !ID_PATTERN.test(id)) {
    errors.id =
      "Use 1-64 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit."
  }
  const cpus = Number(vcpus)
  if (!Number.isInteger(cpus) || cpus < MIN_VCPUS || cpus > MAX_VCPUS) {
    errors.vcpus = `Enter a whole number between ${MIN_VCPUS} and ${MAX_VCPUS}.`
  }
  const memory = Number(memoryMiB)
  if (!Number.isInteger(memory) || memory < MIN_MEMORY_MIB || memory > MAX_MEMORY_MIB) {
    errors.memory_mib = `Enter a whole number of MiB between ${MIN_MEMORY_MIB} and ${MAX_MEMORY_MIB}.`
  }
  if (new TextEncoder().encode(description).length > MAX_DESCRIPTION) {
    errors.description = `Use at most ${MAX_DESCRIPTION} characters.`
  }
  return errors
}

/**
 * InstanceTypeForm creates a type or edits one.
 *
 * On edit the id is shown but not editable (instances reference it), and only
 * the fields the operator changed are sent. That matters for the description:
 * left untouched, the API regenerates a generated description to follow a new
 * size, which a re-sent copy of the old text would prevent.
 */
function InstanceTypeForm(props: InstanceTypeFormProps) {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const initial = props.mode === 'edit' ? props.initial : undefined

  const [id, setId] = useState(initial?.id ?? '')
  const [vcpus, setVcpus] = useState(String(initial?.vcpus ?? 1))
  const [memoryMiB, setMemoryMiB] = useState(String(initial?.memory_mib ?? 1024))
  const [description, setDescription] = useState(initial?.description ?? '')
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})
  const [nothingChanged, setNothingChanged] = useState(false)

  const [createInstanceType, create] = useCreateInstanceTypeMutation()
  const [updateInstanceType, update] = useUpdateInstanceTypeMutation()
  const mutation = props.mode === 'create' ? create : update
  const apiError = mutation.error as ApiError

  const title = initial === undefined ? 'Create instance type' : `Edit ${initial.id}`
  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Instance types', href: '/instance-types' },
      initial === undefined
        ? { text: 'Create instance type', href: '/instance-types/create' }
        : { text: `Edit ${initial.id}`, href: `/instance-types/${initial.id}/edit` },
    ],
  })

  // Local validation wins over the API's: a field fixed and resubmitted must
  // not keep showing the complaint of the previous attempt.
  const errors = { ...fieldErrors(apiError), ...localErrors }

  async function submit(event: FormEvent) {
    event.preventDefault()
    setNothingChanged(false)
    const found = validate(props.mode, id, vcpus, memoryMiB, description)
    setLocalErrors(found)
    if (Object.keys(found).length > 0) {
      return
    }

    try {
      if (initial === undefined) {
        const created = await createInstanceType({
          id,
          vcpus: Number(vcpus),
          memory_mib: Number(memoryMiB),
          ...(description === '' ? {} : { description }),
        }).unwrap()
        dispatch(
          notify({
            type: 'success',
            header: 'Instance type created',
            content: `${created.id} can now be chosen when launching an instance.`,
          }),
        )
      } else {
        const changes = {
          ...(Number(vcpus) === initial.vcpus ? {} : { vcpus: Number(vcpus) }),
          ...(Number(memoryMiB) === initial.memory_mib ? {} : { memory_mib: Number(memoryMiB) }),
          ...(description === initial.description ? {} : { description }),
        }
        if (Object.keys(changes).length === 0) {
          setNothingChanged(true)
          return
        }
        const updated = await updateInstanceType({ id: initial.id, ...changes }).unwrap()
        dispatch(
          notify({
            type: 'success',
            header: 'Instance type updated',
            content: `${updated.id} is now ${updated.vcpus} vCPU, ${formatMemoryMiB(updated.memory_mib)}. Running instances keep the size they launched at.`,
          }),
        )
      }
      navigate('/instance-types')
    } catch {
      // The Alert and the field errors below report it.
    }
  }

  const memoryHint = Number.isInteger(Number(memoryMiB)) ? formatMemoryMiB(Number(memoryMiB)) : ''

  return (
    <form onSubmit={submit}>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description={
              initial === undefined
                ? 'Define a size instances can be launched at.'
                : 'Changes apply to later launches. Instances already running keep the size they launched at.'
            }
          >
            {title}
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="xs">
              <Button variant="link" formAction="none" onClick={() => navigate('/instance-types')}>
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                disabled={!isAdmin}
                loading={mutation.isLoading}
                data-testid="submit-instance-type"
              >
                {initial === undefined ? 'Create instance type' : 'Save changes'}
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {!isAdmin && (
              <Alert type="info" header="Read only">
                Only an administrator can create or change instance types.
              </Alert>
            )}
            {nothingChanged && (
              <Alert type="info" header="Nothing to save">
                No field differs from the stored instance type.
              </Alert>
            )}
            {apiError !== undefined && (
              <Alert type="error" header="The instance type was not saved">
                {errorMessage(apiError, 'The instance type could not be saved.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">Size</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Name"
                  description={
                    initial === undefined
                      ? 'The id instances are launched with, e.g. db.large. It cannot be changed later.'
                      : 'Instances reference the type by this name, so it cannot be changed.'
                  }
                  constraintText={
                    initial === undefined
                      ? "1-64 lowercase letters, digits, '.', '_' or '-'"
                      : undefined
                  }
                  errorText={errors.id}
                >
                  <Input
                    value={id}
                    onChange={({ detail }) => setId(detail.value)}
                    readOnly={initial !== undefined}
                    ariaLabel="Name"
                    autoFocus={initial === undefined}
                  />
                </FormField>

                <FormField
                  label="vCPUs"
                  constraintText={`${MIN_VCPUS}-${MAX_VCPUS}`}
                  errorText={errors.vcpus}
                >
                  <Input
                    type="number"
                    inputMode="numeric"
                    value={vcpus}
                    onChange={({ detail }) => setVcpus(detail.value)}
                    ariaLabel="vCPUs"
                  />
                </FormField>

                <FormField
                  label="Memory (MiB)"
                  description={memoryHint === '' ? undefined : `${memoryHint}`}
                  constraintText={`${MIN_MEMORY_MIB}-${MAX_MEMORY_MIB} MiB; 1024 MiB = 1 GiB`}
                  errorText={errors.memory_mib}
                >
                  <Input
                    type="number"
                    inputMode="numeric"
                    value={memoryMiB}
                    onChange={({ detail }) => setMemoryMiB(detail.value)}
                    ariaLabel="Memory (MiB)"
                  />
                </FormField>

                <FormField
                  label={
                    <span>
                      Description <i>- optional</i>
                    </span>
                  }
                  description="Shown next to the name in the launch form. Left empty, it is generated from the size, e.g. “2 vCPU, 4 GiB”."
                  constraintText={`At most ${MAX_DESCRIPTION} characters`}
                  errorText={errors.description}
                >
                  <Input
                    value={description}
                    onChange={({ detail }) => setDescription(detail.value)}
                    ariaLabel="Description"
                  />
                </FormField>
              </SpaceBetween>
            </Container>
          </SpaceBetween>
        </Form>
      </ContentLayout>
    </form>
  )
}
