import { act, render, screen } from '@testing-library/react'
import { Provider } from 'react-redux'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { createStore, useAppSelector } from '@/app/store'
import { dismiss, notify } from './notificationsSlice'
import { AUTO_DISMISS_MS, useAutoDismiss } from './useAutoDismiss'

/** Harness shows the notifications the way the Flashbar reads them. */
function Harness() {
  const items = useAppSelector((state) => state.notifications.items)
  useAutoDismiss(items)
  return (
    <ul>
      {items.map((item) => (
        <li key={item.id}>{item.content}</li>
      ))}
    </ul>
  )
}

function renderHarness() {
  const store = createStore()
  const view = render(
    <Provider store={store}>
      <Harness />
    </Provider>,
  )
  return { store, ...view }
}

/** tick advances the fake clock and flushes the re-render it causes. */
function tick(ms: number) {
  act(() => {
    vi.advanceTimersByTime(ms)
  })
}

describe('useAutoDismiss', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it.each(['success', 'error', 'warning', 'info', 'in-progress'] as const)(
    'removes a %s notification after five seconds',
    (type) => {
      const { store } = renderHarness()

      act(() => {
        store.dispatch(notify({ type, header: 'Header', content: `${type} content` }))
      })
      expect(screen.getByText(`${type} content`)).toBeInTheDocument()

      tick(AUTO_DISMISS_MS - 1)
      expect(screen.getByText(`${type} content`)).toBeInTheDocument()

      tick(1)
      expect(screen.queryByText(`${type} content`)).not.toBeInTheDocument()
      expect(store.getState().notifications.items).toEqual([])
    },
  )

  it('does not restart an earlier countdown when a later flash arrives', () => {
    const { store } = renderHarness()

    act(() => {
      store.dispatch(notify({ type: 'success', content: 'first' }))
    })
    tick(AUTO_DISMISS_MS - 1_000)
    act(() => {
      store.dispatch(notify({ type: 'success', content: 'second' }))
    })

    // The first flash's clock started when it appeared, not when the second
    // one pushed it, so it goes at its own deadline with the second still up.
    tick(1_000)
    expect(screen.queryByText('first')).not.toBeInTheDocument()
    expect(screen.getByText('second')).toBeInTheDocument()
  })

  it('cancels the countdown when the operator dismisses by hand', () => {
    const { store } = renderHarness()

    act(() => {
      store.dispatch(notify({ type: 'success', content: 'gone early' }))
    })
    act(() => {
      const [first] = store.getState().notifications.items
      if (first === undefined) {
        throw new Error('expected a notification')
      }
      store.dispatch(dismiss(first.id))
    })
    expect(screen.queryByText('gone early')).not.toBeInTheDocument()

    // The deadline passes with nothing left on screen: no timer may fire and
    // push a stale dismissal back into the store.
    tick(AUTO_DISMISS_MS * 2)
    expect(store.getState().notifications.items).toEqual([])
  })

  it('clears every countdown on unmount', () => {
    const { store, unmount } = renderHarness()

    act(() => {
      store.dispatch(notify({ type: 'success', content: 'left behind' }))
    })

    unmount()

    // Nothing is listening any more, so the flash stays where it is instead of
    // being dismissed by a timer the unmounted tree left behind.
    tick(AUTO_DISMISS_MS * 2)
    expect(store.getState().notifications.items).toHaveLength(1)
  })
})
