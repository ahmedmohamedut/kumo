import { describe, expect, it } from 'vitest'

import { clear, dismiss, notificationsReducer, notify } from './notificationsSlice'

const initial = notificationsReducer(undefined, { type: '@@init' })

describe('notificationsSlice', () => {
  it('starts empty', () => {
    expect(initial.items).toEqual([])
  })

  it('adds dismissible notifications with unique ids', () => {
    const withSuccess = notificationsReducer(
      initial,
      notify({ type: 'success', header: 'Done', content: 'Launched' }),
    )
    const withError = notificationsReducer(
      withSuccess,
      notify({ type: 'error', content: 'Failed' }),
    )

    expect(withError.items).toHaveLength(2)
    expect(withError.items[0]).toMatchObject({
      type: 'success',
      header: 'Done',
      content: 'Launched',
      dismissible: true,
    })
    expect(withError.items[0]?.id).not.toBe(withError.items[1]?.id)
  })

  it('keeps a non-dismissible notification', () => {
    const state = notificationsReducer(
      initial,
      notify({ type: 'in-progress', content: 'Importing', dismissible: false }),
    )

    expect(state.items[0]?.dismissible).toBe(false)
  })

  it('dismisses by id and clears everything', () => {
    let state = notificationsReducer(initial, notify({ type: 'info', content: 'One' }))
    state = notificationsReducer(state, notify({ type: 'info', content: 'Two' }))

    const first = state.items[0]
    if (!first) {
      throw new Error('expected a notification')
    }
    state = notificationsReducer(state, dismiss(first.id))
    expect(state.items.map((item) => item.content)).toEqual(['Two'])

    state = notificationsReducer(state, clear())
    expect(state.items).toEqual([])
  })
})
