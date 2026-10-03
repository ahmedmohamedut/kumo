import type { ReactElement } from 'react'
import { render } from '@testing-library/react'
import { Provider } from 'react-redux'
import { MemoryRouter } from 'react-router-dom'
import { configureStore } from '@reduxjs/toolkit'

import { PageMetaProvider } from '@/app/pageMeta'
import { kumoApi } from '@/app/api'
import { notificationsReducer } from '@/features/notifications/notificationsSlice'

/**
 * renderWithProviders renders `ui` with the Redux store, the router and the
 * page meta context the console runs with. Each test gets its own store, so
 * Flashbar items cannot leak between cases.
 */
export function renderWithProviders(ui: ReactElement, { route = '/' }: { route?: string } = {}) {
  const store = configureStore({
    reducer: {
      notifications: notificationsReducer,
      [kumoApi.reducerPath]: kumoApi.reducer,
    },
    middleware: (getDefaultMiddleware) => getDefaultMiddleware().concat(kumoApi.middleware),
  })

  return {
    store,
    ...render(
      <Provider store={store}>
        <PageMetaProvider>
          <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
        </PageMetaProvider>
      </Provider>,
    ),
  }
}
