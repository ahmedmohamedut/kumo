import { describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { RequireAuth } from '@/features/auth/RequireAuth'
import { LoginPage } from '@/features/auth/LoginPage'
import { selectSessionStatus, selectUser } from '@/features/auth/authSlice'
import { renderWithProviders } from '@/test/utils'
import { server } from '@/mocks/server'
import { MOCK_ADMIN, MOCK_ADMIN_PASSWORD } from '@/mocks/handlers'

/** The console the login page is supposed to land in, behind the same gate. */
function Console() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/instances"
        element={
          <RequireAuth>
            <p>Instances</p>
          </RequireAuth>
        }
      />
      <Route
        path="/"
        element={
          <RequireAuth>
            <p>Dashboard</p>
          </RequireAuth>
        }
      />
    </Routes>
  )
}

async function signIn(username: string, password: string) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Username'), username)
  await user.type(screen.getByLabelText('Password'), password)
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
}

describe('LoginPage', () => {
  it('signs an administrator in and opens the page they wanted', async () => {
    const { store } = renderWithProviders(<Console />, { route: '/login?next=%2Finstances' })

    await signIn('ada', MOCK_ADMIN_PASSWORD)

    expect(await screen.findByText('Instances')).toBeInTheDocument()
    expect(selectSessionStatus(store.getState())).toBe('authenticated')
    expect(selectUser(store.getState())).toEqual(MOCK_ADMIN)
  })

  it('reports a wrong password and stays anonymous', async () => {
    const { store } = renderWithProviders(<Console />, { route: '/login?next=%2Finstances' })

    await signIn('ada', 'not the password')

    expect(await screen.findByText('Sign-in failed')).toBeInTheDocument()
    expect(await screen.findByText('the username or password is wrong')).toBeInTheDocument()
    expect(selectSessionStatus(store.getState())).toBe('anonymous')
  })

  it('puts a validation failure on the field that caused it', async () => {
    server.use(
      http.post('/api/v1/auth/login', () =>
        HttpResponse.json(
          {
            error: {
              code: 'invalid_argument',
              message: 'the request is invalid',
              details: [{ field: 'username', reason: 'too short' }],
            },
          },
          { status: 400 },
        ),
      ),
    )
    const { store } = renderWithProviders(<Console />, { route: '/login' })

    await signIn('a', MOCK_ADMIN_PASSWORD)

    expect(await screen.findByText('too short')).toBeInTheDocument()
    expect(selectSessionStatus(store.getState())).toBe('anonymous')
  })

  it('opens the dashboard when there is no next page', async () => {
    renderWithProviders(<Console />, { route: '/login' })

    await signIn('ada', MOCK_ADMIN_PASSWORD)

    expect(await screen.findByText('Dashboard')).toBeInTheDocument()
  })

  it('signs in when Enter is pressed in a field', async () => {
    const user = userEvent.setup()
    renderWithProviders(<Console />, { route: '/login?next=%2Finstances' })

    await user.type(screen.getByLabelText('Username'), MOCK_ADMIN.username)
    // Cloudscape's Form renders no <form>, so this also proves the page's own
    // form element is what makes Enter submit.
    await user.type(screen.getByLabelText('Password'), `${MOCK_ADMIN_PASSWORD}{Enter}`)

    expect(await screen.findByText('Instances')).toBeInTheDocument()
  })

  it('ignores a next page that leaves the console', async () => {
    renderWithProviders(<Console />, { route: '/login?next=https%3A%2F%2Fevil.example' })

    await signIn('ada', MOCK_ADMIN_PASSWORD)

    // An absolute URL would turn the login form into an open redirect.
    expect(await screen.findByText('Dashboard')).toBeInTheDocument()
  })
})
