import { Route, Routes } from 'react-router-dom'
import { http, HttpResponse } from 'msw'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ConsoleLayout } from '@/components/layout/ConsoleLayout'
import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { ImagesListPage } from '@/features/images/ImagesListPage'
import { InstancesListPage } from '@/features/instances/InstancesListPage'
import { KeyPairsListPage } from '@/features/keypairs/KeyPairsListPage'
import { NotFoundPage } from '@/features/system/NotFoundPage'
import { LoginPage } from '@/features/auth/LoginPage'
import { selectSessionStatus, selectUser, signedIn } from '@/features/auth/authSlice'
import { renderWithProviders } from '@/test/utils'
import { server } from '@/mocks/server'
import { MOCK_ADMIN } from '@/mocks/handlers'

function consoleRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/" element={<ConsoleLayout />}>
        <Route index element={<DashboardPage />} />
        <Route path="instances" element={<InstancesListPage />} />
        <Route path="images" element={<ImagesListPage />} />
        <Route path="key-pairs" element={<KeyPairsListPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  )
}

/** signIn puts a session in the store the way a signed-in page load would. */
function signIn(store: { dispatch: (action: unknown) => unknown }) {
  act(() => {
    store.dispatch(signedIn({ user: MOCK_ADMIN, accessToken: 'access-1' }))
  })
}

/**
 * userMenuTrigger is the user menu button. The top navigation renders two
 * copies — the visible one and the offscreen one it measures to decide what
 * fits — and jsdom never fires the ResizeObserver that would collapse them, so
 * the menu is reached through the offscreen copy. Which one a test drives is an
 * implementation detail of Cloudscape, not of the console.
 */
function userMenuTrigger(): HTMLElement {
  const trigger = screen.getAllByLabelText('User menu').find((e) => e.tagName === 'BUTTON')
  if (trigger === undefined) {
    throw new Error('the user menu is missing from the top navigation')
  }
  return trigger
}

/** sideNavigation scopes queries to the AppLayout navigation panel. */
function sideNavigation() {
  return within(screen.getByRole('navigation', { name: 'Console navigation' }))
}

describe('ConsoleLayout', () => {
  it('renders the top and side navigation', () => {
    renderWithProviders(consoleRoutes(), { route: '/' })

    // "Kumo" is the identity of both the top navigation and the side
    // navigation header, so scope each query to its landmark.
    expect(
      within(screen.getByRole('banner')).getByRole('link', { name: 'Kumo' }),
    ).toBeInTheDocument()

    // The top navigation renders its utilities twice (the visible one and the
    // offscreen one it measures), so count matches instead of asserting one.
    for (const label of ['Services', 'User menu', 'This node']) {
      expect(screen.getAllByLabelText(label).length).toBeGreaterThan(0)
    }

    const nav = sideNavigation()
    expect(nav.getByRole('link', { name: 'Kumo' })).toBeInTheDocument()
    expect(nav.getByRole('link', { name: 'Dashboard' })).toBeInTheDocument()
    expect(nav.getByRole('link', { name: 'Instances' })).toBeInTheDocument()
    expect(nav.getByRole('link', { name: 'Images' })).toBeInTheDocument()
    expect(nav.getByRole('link', { name: 'Key pairs' })).toBeInTheDocument()
  })

  it('shows the breadcrumbs the page declares', async () => {
    renderWithProviders(consoleRoutes(), { route: '/images' })

    const breadcrumbs = await screen.findByRole('navigation', { name: 'Breadcrumbs' })
    expect(breadcrumbs).toHaveTextContent('Compute')
    expect(breadcrumbs).toHaveTextContent('Images')
  })

  it('navigates from the side navigation', async () => {
    const user = userEvent.setup()
    renderWithProviders(consoleRoutes(), { route: '/' })

    expect(screen.getByRole('heading', { name: 'Dashboard' })).toBeInTheDocument()

    await user.click(sideNavigation().getByRole('link', { name: 'Key pairs' }))

    expect(await screen.findByRole('heading', { name: 'Key pairs' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Dashboard' })).not.toBeInTheDocument()
  })

  it('marks the active navigation item', () => {
    renderWithProviders(consoleRoutes(), { route: '/instances' })

    expect(sideNavigation().getByRole('link', { name: 'Instances' })).toHaveAttribute(
      'aria-current',
      'page',
    )
    expect(sideNavigation().getByRole('link', { name: 'Images' })).not.toHaveAttribute(
      'aria-current',
    )
  })

  it('renders the not-found page for unknown routes', async () => {
    renderWithProviders(consoleRoutes(), { route: '/does-not-exist' })

    expect(await screen.findByRole('heading', { name: '404' })).toBeInTheDocument()
  })

  it('signs the operator out from the user menu', async () => {
    const user = userEvent.setup()
    const { store } = renderWithProviders(consoleRoutes(), { route: '/' })
    signIn(store)

    await user.click(userMenuTrigger())
    // `hidden` because the menu opens inside the offscreen copy of the top
    // navigation, which jsdom leaves aria-hidden.
    await user.click(await screen.findByRole('menuitem', { name: 'Sign out', hidden: true }))

    expect(await screen.findByRole('heading', { name: 'Sign in to Kumo' })).toBeInTheDocument()
    await waitFor(() => {
      expect(selectSessionStatus(store.getState())).toBe('anonymous')
    })
    expect(selectUser(store.getState())).toBeNull()
  })

  it('ends the session even when the sign-out request is refused', async () => {
    // The cookie is the server's to revoke; the console must not keep pretending
    // to be signed in because the request failed.
    server.use(http.post('/api/v1/auth/logout', () => HttpResponse.json({}, { status: 401 })))
    const user = userEvent.setup()
    const { store } = renderWithProviders(consoleRoutes(), { route: '/' })
    signIn(store)

    await user.click(userMenuTrigger())
    // `hidden` because the menu opens inside the offscreen copy of the top
    // navigation, which jsdom leaves aria-hidden.
    await user.click(await screen.findByRole('menuitem', { name: 'Sign out', hidden: true }))

    expect(await screen.findByRole('heading', { name: 'Sign in to Kumo' })).toBeInTheDocument()
    await waitFor(() => {
      expect(selectSessionStatus(store.getState())).toBe('anonymous')
    })
  })
})
