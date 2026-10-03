import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { RequireAuth } from '@/features/auth/RequireAuth'
import { SetupPage } from '@/features/auth/SetupPage'
import { LoginPage } from '@/features/auth/LoginPage'
import { selectSessionStatus, selectUser } from '@/features/auth/authSlice'
import { renderWithProviders } from '@/test/utils'
import { addMockUser, MOCK_ADMIN, setMockNeedsSetup } from '@/mocks/handlers'

function Console() {
  return (
    <Routes>
      <Route path="/setup" element={<SetupPage />} />
      <Route path="/login" element={<LoginPage />} />
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

async function createAdministrator(username: string, password: string, confirmation: string) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Username'), username)
  await user.type(screen.getByLabelText('Password'), password)
  await user.type(screen.getByLabelText('Confirm password'), confirmation)
  await user.click(screen.getByRole('button', { name: 'Create administrator' }))
}

describe('SetupPage', () => {
  it('creates the first administrator and signs them in', async () => {
    setMockNeedsSetup(true)
    addMockUser(MOCK_ADMIN, 'correct horse battery staple')
    const { store } = renderWithProviders(<Console />, { route: '/setup' })

    await createAdministrator('ada', 'correct horse battery staple', 'correct horse battery staple')

    expect(await screen.findByText('Dashboard')).toBeInTheDocument()
    expect(selectSessionStatus(store.getState())).toBe('authenticated')
    expect(selectUser(store.getState())?.role).toBe('admin')
  })

  it('refuses a password that is too short, before the request', async () => {
    setMockNeedsSetup(true)
    addMockUser(MOCK_ADMIN, 'correct horse battery staple')
    const { store } = renderWithProviders(<Console />, { route: '/setup' })

    await createAdministrator('ada', 'short', 'short')

    expect(await screen.findByText('Use at least 12 characters.')).toBeInTheDocument()
    expect(selectSessionStatus(store.getState())).not.toBe('authenticated')
  })

  it('refuses a confirmation that does not match', async () => {
    setMockNeedsSetup(true)
    addMockUser(MOCK_ADMIN, 'correct horse battery staple')
    renderWithProviders(<Console />, { route: '/setup' })

    await createAdministrator('ada', 'correct horse battery staple', 'something else')

    expect(await screen.findByText('The passwords do not match.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Create administrator' })).toBeDisabled()
  })

  it('sends an operator to the login form once setup is closed', async () => {
    renderWithProviders(<Console />, { route: '/setup' })

    expect(await screen.findByRole('heading', { name: 'Sign in to Kumo' })).toBeInTheDocument()
  })
})
