import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { Provider } from 'react-redux'
import { RouterProvider } from 'react-router-dom'
import '@cloudscape-design/global-styles/index.css'

import { PageMetaProvider } from '@/app/pageMeta'
import { router } from '@/app/router'
import { store } from '@/app/store'
import { applyStoredMode } from '@/app/theme'

applyStoredMode()

const container = document.getElementById('root')
if (!container) {
  throw new Error('the #root element is missing from index.html')
}

createRoot(container).render(
  <StrictMode>
    <Provider store={store}>
      <PageMetaProvider>
        <RouterProvider router={router} />
      </PageMetaProvider>
    </Provider>
  </StrictMode>,
)
