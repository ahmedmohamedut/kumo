import { useState, type FormEvent } from 'react'
import { Navigate, useNavigate } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Form from '@cloudscape-design/components/form'
import FormField from '@cloudscape-design/components/form-field'
import Input from '@cloudscape-design/components/input'
import ProgressBar from '@cloudscape-design/components/progress-bar'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { AuthPage } from '@/features/auth/AuthPage'
import { useLoginMutation, useSetupMutation, useSystemInfoQuery } from '@/features/auth/api'
import { errorMessage, fieldErrors } from '@/lib/apiError'

/** minLength mirrors the backend policy, so the form fails before the request. */
const MIN_PASSWORD_LENGTH = 12

/**
 * SetupPage creates the very first administrator of a fresh instance.
 *
 * It is reachable only while `/system/info` reports `needs_setup`; once the
 * instance has a user the page sends the operator to the login form, because
 * the one-time setup path must not become a way to add accounts.
 */
export function SetupPage() {
  const navigate = useNavigate()
  const info = useSystemInfoQuery()
  const [createAccount, setup] = useSetupMutation()
  const [signIn, login] = useLoginMutation()

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirmation, setConfirmation] = useState('')

  if (!info.isLoading && info.data !== undefined && !info.data.needs_setup) {
    return <Navigate replace to="/login" />
  }

  const busy = setup.isLoading || login.isLoading
  const error = setup.error ?? login.error
  const fieldError = fieldErrors(error)

  const mismatch = confirmation.length > 0 && password !== confirmation
  const tooShort = password.length > 0 && password.length < MIN_PASSWORD_LENGTH

  // Cloudscape's Form renders no <form> of its own, so the page supplies one;
  // that is what makes the button and Enter in a field submit here.
  async function create(event: FormEvent) {
    event.preventDefault()
    if (password.length < MIN_PASSWORD_LENGTH || password !== confirmation) {
      return
    }
    const credentials = { username, password }
    try {
      await createAccount(credentials).unwrap()
      // /auth/setup only creates the administrator; signing in here means the
      // operator lands in the console instead of on a login form they have just
      // filled in twice.
      await signIn(credentials).unwrap()
      navigate('/', { replace: true })
    } catch {
      // Reported by the Alert and the field errors below.
    }
  }

  return (
    <AuthPage
      title="Set up Kumo"
      description="Create the administrator account for this instance. It is the only account for now; more can be added later with kumod admin create."
    >
      <form onSubmit={create}>
        <Form
          actions={
            <Box float="right">
              <Button
                variant="primary"
                formAction="submit"
                loading={busy}
                disabled={mismatch || tooShort}
                data-testid="create-administrator"
              >
                Create administrator
              </Button>
            </Box>
          }
        >
          <SpaceBetween size="m">
            {info.error !== undefined && (
              <Alert type="error" header="Cannot read the instance state">
                {errorMessage(info.error, 'The state of this instance is unknown.')}
              </Alert>
            )}
            {error !== undefined && (
              <Alert type="error" header="Setup failed">
                {errorMessage(error, 'The administrator could not be created.')}
              </Alert>
            )}
            {busy && <ProgressBar status="in-progress" ariaLabel="Creating the administrator" />}
            <FormField label="Username" errorText={fieldError.username}>
              <Input
                value={username}
                onChange={({ detail }) => setUsername(detail.value)}
                autoComplete="username"
                ariaLabel="Username"
              />
            </FormField>
            <FormField
              label="Password"
              errorText={
                fieldError.password ??
                (tooShort ? `Use at least ${MIN_PASSWORD_LENGTH} characters.` : undefined)
              }
              constraintText={`${password.length}/${MIN_PASSWORD_LENGTH} characters`}
            >
              <Input
                type="password"
                value={password}
                onChange={({ detail }) => setPassword(detail.value)}
                autoComplete="new-password"
                ariaLabel="Password"
              />
            </FormField>
            <FormField
              label="Confirm password"
              errorText={mismatch ? 'The passwords do not match.' : undefined}
            >
              <Input
                type="password"
                value={confirmation}
                onChange={({ detail }) => setConfirmation(detail.value)}
                autoComplete="new-password"
                ariaLabel="Confirm password"
              />
            </FormField>
          </SpaceBetween>
        </Form>
      </form>
    </AuthPage>
  )
}
