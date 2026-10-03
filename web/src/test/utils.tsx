import type { ReactElement } from 'react'
import { render } from '@testing-library/react'
import { Provider } from 'react-redux'
import { MemoryRouter } from 'react-router-dom'

import { PageMetaProvider } from '@/app/pageMeta'
import { createStore } from '@/app/store'
import { authApi } from '@/features/auth/api'
import { SessionBootstrap } from '@/features/auth/SessionBootstrap'
import { MOCK_ADMIN, MOCK_ADMIN_PASSWORD, MOCK_VIEWER } from '@/mocks/handlers'

/**
 * renderWithProviders renders `ui` with the Redux store, the router and the
 * page meta context the console runs with, plus the session bootstrap
 * `main.tsx` mounts — so a test sees the same first render as the browser.
 * Each test gets its own store, so Flashbar items and sessions cannot leak
 * between cases.
 */
export function renderWithProviders(ui: ReactElement, { route = '/' }: { route?: string } = {}) {
  const store = createStore()

  return {
    store,
    ...render(
      <Provider store={store}>
        <PageMetaProvider>
          <SessionBootstrap />
          <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
        </PageMetaProvider>
      </Provider>,
    ),
  }
}

/**
 * signIn establishes a session through the mocked login endpoint, on a throwaway
 * store.
 *
 * What survives is the refresh cookie MSW keeps in its jar, which is exactly the
 * situation the browser is in after a login: the store `renderWithProviders`
 * builds starts empty and `SessionBootstrap` restores the session from that
 * cookie alone. Signing in this way keeps a page test from having to drive the
 * login form first.
 */
export async function signInAs(user: typeof MOCK_ADMIN | typeof MOCK_VIEWER = MOCK_ADMIN) {
  const password =
    user.username === MOCK_ADMIN.username ? MOCK_ADMIN_PASSWORD : 'a long enough passphrase'
  const store = createStore()
  await store
    .dispatch(authApi.endpoints.login.initiate({ username: user.username, password }))
    .unwrap()
}
