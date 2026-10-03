import { createSlice, type PayloadAction } from '@reduxjs/toolkit'

/** A Flashbar item, mirroring Cloudscape's `FlashMessageProps`. */
export type NotificationType = 'success' | 'error' | 'warning' | 'info' | 'in-progress'

export interface Notification {
  id: string
  type: NotificationType
  header?: string
  content: string
  dismissible: boolean
}

export interface NotificationsState {
  items: Notification[]
}

const initialState: NotificationsState = { items: [] }

let nextId = 0

const notificationsSlice = createSlice({
  name: 'notifications',
  initialState,
  reducers: {
    /** Shows a notification. Every item is dismissible unless stated otherwise. */
    notify: {
      reducer: (state, action: PayloadAction<Notification>) => {
        state.items.push(action.payload)
      },
      prepare: (
        notification: Omit<Notification, 'id' | 'dismissible'> & { dismissible?: boolean },
      ) => ({
        payload: {
          dismissible: true,
          ...notification,
          id: `notification-${(nextId += 1)}`,
        } satisfies Notification,
      }),
    },
    /** Removes a notification by id. */
    dismiss: (state, action: PayloadAction<string>) => {
      state.items = state.items.filter((item) => item.id !== action.payload)
    },
    /** Removes every notification, e.g. on navigation or logout. */
    clear: (state) => {
      state.items = []
    },
  },
})

export const { notify, dismiss, clear } = notificationsSlice.actions
export const notificationsReducer = notificationsSlice.reducer
