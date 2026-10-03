import { useEffect, type ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import Box from '@cloudscape-design/components/box'
import Spinner from '@cloudscape-design/components/spinner'

import { refreshSession } from '@/app/authSession'
import { selectSessionStatus } from '@/features/auth/authSlice'
import { useAppDispatch, useAppSelector } from '@/app/store'

/**
 * RequireAuth is the gate in front of every console route (03-frontend.md §3).
 *
 * While the session is still `unknown` it shows a spinner instead of
 * redirecting: the first render of a reload happens before the refresh cookie
 * has been spent, and redirecting there would throw a signed-in operator to the
 * login page on every reload.
 *
 * The `next` query parameter carries where they were, so signing in returns
 * them to the page they asked for instead of the dashboard.
 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const dispatch = useAppDispatch()
  const status = useAppSelector(selectSessionStatus)
  const location = useLocation()

  useEffect(() => {
    if (status === 'unknown') {
      void refreshSession(dispatch)
    }
  }, [dispatch, status])

  if (status === 'unknown') {
    return (
      <Box textAlign="center" padding={{ top: 'xxl' }}>
        {/* A spinner with no name is invisible to a screen reader. */}
        <div role="status" aria-label="Checking the session">
          <Spinner size="large" />
        </div>
      </Box>
    )
  }

  if (status === 'anonymous') {
    const next = `${location.pathname}${location.search}`
    return <Navigate replace to={`/login?next=${encodeURIComponent(next)}`} />
  }

  return children
}
