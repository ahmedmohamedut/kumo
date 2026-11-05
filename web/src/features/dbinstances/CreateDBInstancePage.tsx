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
import { useCreateDBInstanceMutation } from '@/features/dbinstances/api'
import { DatabasesGate, NoEngineImageAlert, useEngines } from '@/features/dbinstances/DatabasesGate'
import {
  identifierProblem,
  MAX_PASSWORD_LENGTH,
  MIN_PASSWORD_LENGTH,
  MIN_STORAGE_GB,
  passwordProblem,
  pgNameProblem,
  storageProblem,
} from '@/features/dbinstances/state'
import { useListInstanceTypesQuery } from '@/features/instancetypes/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

/** CreateDBInstancePage is the Create database form. */
export function CreateDBInstancePage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Databases', href: '/databases' },
      { text: 'Databases', href: '/databases' },
      { text: 'Create database', href: '/databases/create' },
    ],
  })

  return (
    <DatabasesGate title="Create database">
      <CreateDBInstanceForm />
    </DatabasesGate>
  )
}

/**
 * CreateDBInstanceForm creates a DB instance.
 *
 * The password is the part worth reading carefully. Kumo derives a SCRAM
 * verifier from it and discards it, so there is no password reset and no way
 * to show it again (07-databases.md R8) — the form says so next to the field,
 * because a user who loses it has to create the database again.
 */
function CreateDBInstanceForm() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { engines } = useEngines()
  const { data: types } = useListInstanceTypesQuery()

  const [identifier, setIdentifier] = useState('')
  const [engineVersion, setEngineVersion] = useState<SelectProps.Option | null>(null)
  const [instanceType, setInstanceType] = useState<SelectProps.Option | null>(null)
  const [storageGB, setStorageGB] = useState('20')
  const [adminUsername, setAdminUsername] = useState('')
  const [adminPassword, setAdminPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [databaseName, setDatabaseName] = useState('')
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})

  const [createDBInstance, { isLoading }] = useCreateDBInstanceMutation()

  const errors = { ...fieldErrors(error), ...localErrors }

  const engineOptions: SelectProps.Options = engines.map((engine) => ({
    value: engine.version,
    label: `PostgreSQL ${engine.version}`,
  }))
  const typeOptions: SelectProps.Options = (types ?? []).map((type) => ({
    value: type.id,
    label: type.id,
    description: `${type.vcpus} vCPU, ${type.memory_mib} MiB memory`,
  }))

  // The newest engine and the smallest type are what an operator gets by
  // default; both are a single click to change.
  const engineChoice = engineVersion ?? engineOptions[0] ?? null
  const typeChoice = instanceType ?? typeOptions[0] ?? null

  function validate(): Record<string, string> {
    const invalid: Record<string, string> = {}

    const identifierIssue = identifierProblem(identifier.trim())
    if (identifierIssue !== null) {
      invalid['identifier'] = identifierIssue
    }
    const usernameIssue = pgNameProblem(adminUsername.trim(), 'a name for the admin user')
    if (usernameIssue !== null) {
      invalid['admin_username'] = usernameIssue
    }
    const databaseIssue = pgNameProblem(databaseName.trim(), 'a name for the initial database')
    if (databaseIssue !== null) {
      invalid['database_name'] = databaseIssue
    }
    const passwordIssue = passwordProblem(adminPassword)
    if (passwordIssue !== null) {
      invalid['admin_password'] = passwordIssue
    } else if (confirmPassword !== adminPassword) {
      invalid['confirm_password'] = 'The two passwords do not match.'
    }
    const storageIssue = storageProblem(Number(storageGB))
    if (storageIssue !== null) {
      invalid['storage_gb'] = storageIssue
    }
    if (typeChoice?.value === undefined) {
      invalid['instance_type'] = 'Choose a size for the server.'
    }

    return invalid
  }

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError(undefined)

    const invalid = validate()
    setLocalErrors(invalid)
    if (Object.keys(invalid).length > 0) {
      return
    }

    try {
      const created = await createDBInstance({
        identifier: identifier.trim(),
        engine: 'postgres',
        ...(engineChoice?.value === undefined ? {} : { engine_version: engineChoice.value }),
        instance_type: String(typeChoice?.value),
        storage_gb: Number(storageGB),
        admin_username: adminUsername.trim(),
        admin_password: adminPassword,
        database_name: databaseName.trim(),
      }).unwrap()
      dispatch(
        notify({
          type: 'in-progress',
          header: 'Database creation started',
          content: `${created.identifier} is being created. It is ready to connect to once it is available.`,
        }),
      )
      navigate(`/databases/${created.id}`)
    } catch (err) {
      setError(err as ApiError)
    }
  }

  if (engines.length === 0) {
    return (
      <ContentLayout header={<Header variant="h1">Create database</Header>}>
        <NoEngineImageAlert />
      </ContentLayout>
    )
  }

  return (
    <form onSubmit={submit}>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description="A Postgres server Kumo runs on this node, reachable from inside its VPC."
          >
            Create database
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/databases')} formAction="none">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isLoading}
                loadingText="Creating"
                disabled={!isAdmin}
                data-testid="submit-create-db-instance"
              >
                Create database
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {!isAdmin && <Alert type="info">Only an administrator can create databases.</Alert>}
            {error !== undefined && Object.keys(fieldErrors(error)).length === 0 && (
              <Alert type="error" header="The database was not created">
                {errorMessage(error, 'The database could not be created.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">Engine</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Engine version"
                  description="The engine image this node has. A patched image is imported alongside the old one, and new databases take the newest."
                >
                  <Select
                    selectedOption={engineChoice}
                    options={engineOptions}
                    onChange={({ detail }) => setEngineVersion(detail.selectedOption)}
                    ariaLabel="Engine version"
                  />
                </FormField>

                <FormField
                  label="DB instance identifier"
                  description="Unique on this node, and the name the database answers on inside its VPC. It cannot be changed later."
                  constraintText="1-60 characters: a letter, then lowercase letters, digits and single dashes."
                  errorText={errors['identifier']}
                >
                  <Input
                    value={identifier}
                    onChange={({ detail }) => setIdentifier(detail.value)}
                    ariaLabel="DB instance identifier"
                    placeholder="orders"
                    autoFocus
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h2">Credentials</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Admin user"
                  description="The Postgres role that owns the initial database."
                  constraintText="Lowercase letters, digits and underscores. Names Postgres reserves are refused."
                  errorText={errors['admin_username']}
                >
                  <Input
                    value={adminUsername}
                    onChange={({ detail }) => setAdminUsername(detail.value)}
                    ariaLabel="Admin user"
                    placeholder="app"
                  />
                </FormField>

                <FormField
                  label="Password"
                  description="Kumo does not keep this password. It derives the engine's credential from it and discards it, so it cannot be shown again or reset: a database whose password is lost has to be created again."
                  constraintText={`${MIN_PASSWORD_LENGTH}-${MAX_PASSWORD_LENGTH} printable ASCII characters.`}
                  errorText={errors['admin_password']}
                >
                  <Input
                    type="password"
                    value={adminPassword}
                    onChange={({ detail }) => setAdminPassword(detail.value)}
                    ariaLabel="Password"
                  />
                </FormField>

                <FormField label="Confirm password" errorText={errors['confirm_password']}>
                  <Input
                    type="password"
                    value={confirmPassword}
                    onChange={({ detail }) => setConfirmPassword(detail.value)}
                    ariaLabel="Confirm password"
                  />
                </FormField>

                <Alert type="info" header="Save this password somewhere safe">
                  There is no password reset in this release. Kumo never stores the password, in any
                  form, so nothing on this node can recover it or change it later.
                </Alert>

                <FormField
                  label="Initial database"
                  description="Created at first boot and owned by the admin user."
                  errorText={errors['database_name']}
                >
                  <Input
                    value={databaseName}
                    onChange={({ detail }) => setDatabaseName(detail.value)}
                    ariaLabel="Initial database"
                    placeholder="orders"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h2">Size and storage</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="DB instance size"
                  description="The vCPUs and memory of the server. The engine's buffers are sized from the memory at first boot."
                  errorText={errors['instance_type']}
                >
                  <Select
                    selectedOption={typeChoice}
                    options={typeOptions}
                    onChange={({ detail }) => setInstanceType(detail.selectedOption)}
                    ariaLabel="DB instance size"
                  />
                </FormField>

                <FormField
                  label="Storage"
                  description="A disk of its own for the data directory, so the engine image can be replaced later without touching the data."
                  constraintText={`At least ${MIN_STORAGE_GB} GiB. It cannot be grown in this release.`}
                  errorText={errors['storage_gb']}
                >
                  <Input
                    type="number"
                    value={storageGB}
                    onChange={({ detail }) => setStorageGB(detail.value)}
                    ariaLabel="Storage in GiB"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Alert type="warning" header="Connections are not encrypted">
              A DB instance accepts plain connections from inside its VPC and is not reachable from
              outside it. There is no TLS in this release, so treat the VPC as the boundary:{' '}
              <Box variant="code">sslmode=require</Box> will not connect.
            </Alert>
          </SpaceBetween>
        </Form>
      </ContentLayout>
    </form>
  )
}
