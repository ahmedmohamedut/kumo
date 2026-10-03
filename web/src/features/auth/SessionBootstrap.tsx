import { useEffect } from 'react'

import { refreshSession } from '@/app/authSession'
import { selectSessionStatus } from '@/features/auth/authSlice'
import { useAppDispatch, useAppSelector } from '@/app/store'

/**
 * SessionBootstrap is the "on app start, call POST /auth/refresh" of
 * 03-frontend.md §4. It renders nothing: the protected routes wait for the
 * status it settles on.
 *
 * `RequireAuth` bootstraps too when nobody has, so a console rendered without
 * this (a test, an embed) still resolves its session; `refreshSession` is
 * single-flight, so the two can never overlap.
 */
export function SessionBootstrap() {
  const dispatch = useAppDispatch()
  const status = useAppSelector(selectSessionStatus)

  useEffect(() => {
    if (status === 'unknown') {
      void refreshSession(dispatch)
    }
  }, [dispatch, status])

  return null
}
