import { configureStore } from '@reduxjs/toolkit'
import { setupListeners } from '@reduxjs/toolkit/query'
import { useDispatch, useSelector } from 'react-redux'

import { kumoApi } from '@/app/api'
import { notificationsReducer } from '@/features/notifications/notificationsSlice'

export const store = configureStore({
  reducer: {
    notifications: notificationsReducer,
    [kumoApi.reducerPath]: kumoApi.reducer,
  },
  middleware: (getDefaultMiddleware) => getDefaultMiddleware().concat(kumoApi.middleware),
})

// Enables refetchOnFocus and refetchOnReconnect behaviour for RTK Query.
setupListeners(store.dispatch)

export type RootState = ReturnType<typeof store.getState>
export type AppDispatch = typeof store.dispatch

export const useAppDispatch = useDispatch.withTypes<AppDispatch>()
export const useAppSelector = useSelector.withTypes<RootState>()
