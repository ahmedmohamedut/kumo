import { useEffect, useRef } from 'react'
import { useAppDispatch } from '@/app/store'
import { dismiss, type Notification } from './notificationsSlice'

/**
 * How long a flash stays on screen. The header and the resource id are what an
 * operator reads, and both fit comfortably in five seconds; after that the
 * flash is noise covering the table it is talking about.
 */
export const AUTO_DISMISS_MS = 5_000

/**
 * useAutoDismiss drops notifications on a timer so the Flashbar does not pile
 * up over a session. Every type goes, errors and warnings included.
 *
 * The countdown is kept per notification id using a Map in a Ref.
 * This ensures we don't restart timers on every render, and that manual
 * dismissals cancel their respective timers.
 */
export function useAutoDismiss(items: readonly Notification[]): void {
  const dispatch = useAppDispatch()
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>())

  useEffect(() => {
    const currentIds = new Set(items.map((item) => item.id))

    // 1. Cleanup timers for notifications that have been manually dismissed or removed
    for (const [id, timer] of timers.current.entries()) {
      if (!currentIds.has(id)) {
        clearTimeout(timer)
        timers.current.delete(id)
      }
    }

    // 2. Schedule dismissal for new notifications
    for (const item of items) {
      if (!timers.current.has(item.id)) {
        const timer = setTimeout(() => {
          dispatch(dismiss(item.id))
          timers.current.delete(item.id)
        }, AUTO_DISMISS_MS)
        timers.current.set(item.id, timer)
      }
    }
  }, [items, dispatch])

  // 3. Cleanup all timers when the component unmounts
  useEffect(() => {
    return () => {
      for (const timer of timers.current.values()) {
        clearTimeout(timer)
      }
      timers.current.clear()
    }
  }, [])
}
