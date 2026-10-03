import { useState, type FormEvent } from 'react'
import { Navigate, useNavigate, useSearchParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Form from '@cloudscape-design/components/form'
import FormField from '@cloudscape-design/components/form-field'
import Input from '@cloudscape-design/components/input'
import SpaceBetween from '@cloudscape-design/components/space-between'

import { AuthPage } from '@/features/auth/AuthPage'
import { useLoginMutation } from '@/features/auth/api'
import { selectUser } from '@/features/auth/authSlice'
import { useAppSelector } from '@/app/store'
import { errorMessage, fieldErrors } from '@/lib/apiError'

/** nextPath is the page the operator was heading for, if RequireAuth sent one. */
function nextPath(search: URLSearchParams): string {
  const next = search.get('next')
  // Only in-app paths: an absolute URL here would turn the login page into an
  // open redirect.
  return next !== null && next.startsWith('/') ? next : '/'
}

/**
 * LoginPage signs an operator in. The password never leaves this form except in
 * the request body; the refresh token arrives as an HttpOnly cookie and the
 * access token lives in the auth slice (03-frontend.md §4).
 */
export function LoginPage() {
  const user = useAppSelector(selectUser)
  const navigate = useNavigate()
  const [search] = useSearchParams()
  const [login, { isLoading, error }] = useLoginMutation()

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')

  if (user !== null) {
    return <Navigate replace to={nextPath(search)} />
  }

  // Cloudscape's Form renders no <form> of its own, so the page supplies one;
  // that is what makes the button and Enter in a field submit here.
  async function signIn(event: FormEvent) {
    event.preventDefault()
    try {
      await login({ username, password }).unwrap()
      navigate(nextPath(search), { replace: true })
    } catch {
      // The Alert below reports it; the rejected promise is already expected.
    }
  }

  const fieldError = fieldErrors(error)

  return (
    <AuthPage
      title="Sign in to Kumo"
      description="Kumo is the private cloud control plane for this node."
    >
      <form onSubmit={signIn}>
        <Form
          actions={
            <Box float="right">
              <Button
                variant="primary"
                formAction="submit"
                loading={isLoading}
                data-testid="sign-in"
              >
                Sign in
              </Button>
            </Box>
          }
        >
          <SpaceBetween size="m">
            {error !== undefined && (
              <Alert type="error" header="Sign-in failed">
                {errorMessage(error, 'The username or password is wrong.')}
              </Alert>
            )}
            <FormField
              label="Username"
              errorText={fieldError.username}
              constraintText="The account created by `kumod admin create`."
            >
              <Input
                value={username}
                onChange={({ detail }) => setUsername(detail.value)}
                autoComplete="username"
                autoFocus
                ariaLabel="Username"
              />
            </FormField>
            <FormField label="Password" errorText={fieldError.password}>
              <Input
                type="password"
                value={password}
                onChange={({ detail }) => setPassword(detail.value)}
                autoComplete="current-password"
                ariaLabel="Password"
              />
            </FormField>
          </SpaceBetween>
        </Form>
      </form>
      <Box variant="small" padding={{ top: 'l' }}>
        Passwords are the only credentials in Kumo: there are no API keys yet.
      </Box>
    </AuthPage>
  )
}
