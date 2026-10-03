import { configureStore } from '@reduxjs/toolkit'
import { setupListeners } from '@reduxjs/toolkit/query'
import { useDispatch, useSelector } from 'react-redux'

import { kumoApi } from '@/app/api'
import { authReducer } from '@/features/auth/authSlice'
import { notificationsReducer } from '@/features/notifications/notificationsSlice'

/**
 * createStore builds a store with the console's reducers. Tests build their own
 * per test (a Flashbar item or a session must not leak between cases), so the
 * shape lives in one place instead of being copied into the test helpers.
 */
export function createStore() {
  return configureStore({
    reducer: {
      auth: authReducer,
      notifications: notificationsReducer,
      [kumoApi.reducerPath]: kumoApi.reducer,
    },
    middleware: (getDefaultMiddleware) => getDefaultMiddleware().concat(kumoApi.middleware),
  })
}

export const store = createStore()

// Enables refetchOnFocus and refetchOnReconnect behaviour for RTK Query.
setupListeners(store.dispatch)

export type RootState = ReturnType<typeof store.getState>
export type AppDispatch = typeof store.dispatch

export const useAppDispatch = useDispatch.withTypes<AppDispatch>()
export const useAppSelector = useSelector.withTypes<RootState>()
