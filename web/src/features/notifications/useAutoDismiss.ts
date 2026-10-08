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
 * up over a session. Every type goes, errors and warnings included: a failure
 * that sits there for the rest of the day stops being read, and the tasks page
 * is where what did not work is still visible.
 *
 * The countdown is kept per notification id rather than restarted on every
 * render, so pushing a second flash does not extend the first one's stay, and a
 * notification dismissed by hand cancels its own timer instead of leaving one
 * behind to dispatch later.
 */
export function useAutoDismiss(items: readonly Notification[]): void {
  const dispatch = useAppDispatch()
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>())

  useEffect(() => {
    const pending = timers.current

    // Forget countdowns for notifications that are no longer in the store, so
    // a stale timer cannot dismiss a later notification that reused the slot.
    for (const [id, timer] of pending) {
      if (!items.some((item) => item.id === id)) {
        clearTimeout(timer)
        pending.delete(id)
      }
    }

    for (const item of items) {
      if (pending.has(item.id)) {
        continue
      }
      pending.set(
        item.id,
        setTimeout(() => {
          pending.delete(item.id)
          dispatch(dismiss(item.id))
        }, AUTO_DISMISS_MS),
      )
    }
  }, [items, dispatch])

  // Unmounting (sign out, test teardown) must not leave timers running against
  // a store nothing renders any more. This cleanup is deliberately separate
  // from the one above: returning it from the effect above would clear every
  // countdown whenever the list changes, restarting them all.
  useEffect(() => {
    const pending = timers.current
    return () => {
      for (const timer of pending.values()) {
        clearTimeout(timer)
      }
      pending.clear()
    }
  }, [])
}
