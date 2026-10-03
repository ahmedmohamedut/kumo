import { describe, expect, it } from 'vitest'
import { cleanup, screen } from '@testing-library/react'
import { Route, Routes, useLocation } from 'react-router-dom'
import userEvent from '@testing-library/user-event'

import { RequireAuth } from '@/features/auth/RequireAuth'
import { LoginPage } from '@/features/auth/LoginPage'
import { SessionBootstrap } from '@/features/auth/SessionBootstrap'
import { authApi } from '@/features/auth/api'
import { selectSessionStatus, selectUser } from '@/features/auth/authSlice'
import { createStore } from '@/app/store'
import { renderWithProviders } from '@/test/utils'
import { MOCK_ADMIN, MOCK_ADMIN_PASSWORD } from '@/mocks/handlers'

/** Instances echoes its query string so a test can see where it landed. */
function Instances() {
  const location = useLocation()
  return <p>{`Instances ${location.search}`}</p>
}

/** A console page behind the gate, and the login form it redirects to. */
function Console() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/instances"
        element={
          <RequireAuth>
            <Instances />
          </RequireAuth>
        }
      />
    </Routes>
  )
}

async function signIn() {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Username'), MOCK_ADMIN.username)
  await user.type(screen.getByLabelText('Password'), MOCK_ADMIN_PASSWORD)
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
}

describe('the session gate', () => {
  it('keeps the session across a reload, using only the cookie', async () => {
    // A first page load signs in: the store holds the token, the browser holds
    // the cookie.
    const browser = createStore()
    await browser
      .dispatch(
        authApi.endpoints.login.initiate({
          username: MOCK_ADMIN.username,
          password: MOCK_ADMIN_PASSWORD,
        }),
      )
      .unwrap()
    expect(selectSessionStatus(browser.getState())).toBe('authenticated')
    cleanup()

    // A reload throws the store — and with it the access token — away. The
    // cookie is the only thing that survives.
    const reloaded = renderWithProviders(<Console />, { route: '/instances' })

    expect(await screen.findByText('Instances')).toBeInTheDocument()
    expect(selectSessionStatus(reloaded.store.getState())).toBe('authenticated')
    expect(selectUser(reloaded.store.getState())).toEqual(MOCK_ADMIN)
    expect(reloaded.store.getState().auth.accessToken).not.toBeNull()
  })

  it('sends an anonymous visitor to the login form, keeping the page they wanted', async () => {
    const { store } = renderWithProviders(<Console />, { route: '/instances' })

    expect(await screen.findByRole('heading', { name: 'Sign in to Kumo' })).toBeInTheDocument()
    expect(selectSessionStatus(store.getState())).toBe('anonymous')
  })

  it('carries the query string of the page it interrupted', async () => {
    renderWithProviders(<Console />, { route: '/instances?state=running' })

    expect(await screen.findByRole('heading', { name: 'Sign in to Kumo' })).toBeInTheDocument()

    await signIn()

    expect(await screen.findByText('Instances ?state=running')).toBeInTheDocument()
  })

  it('shows a spinner instead of the login form while the cookie is being spent', () => {
    renderWithProviders(
      <>
        <SessionBootstrap />
        <Console />
      </>,
      { route: '/instances' },
    )

    // The first render must not decide anything: the refresh has not answered
    // yet, and a redirect here would throw a signed-in operator out on every
    // reload.
    expect(screen.getByLabelText('Checking the session')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Sign in to Kumo' })).not.toBeInTheDocument()
  })
})
