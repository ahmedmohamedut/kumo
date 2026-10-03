import type { ReactElement } from 'react'
import { render } from '@testing-library/react'
import { Provider } from 'react-redux'
import { MemoryRouter } from 'react-router-dom'

import { PageMetaProvider } from '@/app/pageMeta'
import { createStore } from '@/app/store'
import { SessionBootstrap } from '@/features/auth/SessionBootstrap'

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
