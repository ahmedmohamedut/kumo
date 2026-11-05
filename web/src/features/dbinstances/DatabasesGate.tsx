import type { ReactNode } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Header from '@cloudscape-design/components/header'
import Link from '@cloudscape-design/components/link'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'

import { useSystemInfoQuery } from '@/features/auth/api'

/**
 * DatabasesGate renders the Databases pages only on a node with the module on
 * (07-databases.md §2). With it off, every `/rds` route answers 503, so the
 * page explains how to turn it on instead of rendering a table that can only
 * fail.
 *
 * A node with the module on and no engine image is **not** gated: that is the
 * normal state of a fresh install, the list is simply empty, and the banner
 * below explains the one import an operator has to do. Gating it would hide a
 * working module behind a missing file.
 */
export function DatabasesGate({ title, children }: { title: string; children: ReactNode }) {
  const { data: info, isLoading } = useSystemInfoQuery()

  if (isLoading) {
    return <Spinner size="large" />
  }

  if (info !== undefined && !info.databases.enabled) {
    return (
      <ContentLayout header={<Header variant="h1">{title}</Header>}>
        <Alert type="info" header="Databases are turned off on this node">
          <SpaceBetween size="xs">
            <span>
              Kumo runs managed Postgres servers as virtual machines on this node. To turn the
              module on, set <Box variant="code">KUMO_DATABASES=on</Box> in{' '}
              <Box variant="code">kumod.env</Box> and restart kumod.
            </span>
            <span>
              It needs no other server: what it does need is an engine image, which is imported like
              any other image.
            </span>
          </SpaceBetween>
        </Alert>
      </ContentLayout>
    )
  }

  return <SpaceBetween size="m">{children}</SpaceBetween>
}

/**
 * useEngines reports what this node can create a DB instance from. The list is
 * empty until an operator imports an engine image, which is a state the
 * console explains rather than a fault (07-databases.md §2).
 */
export function useEngines() {
  const { data: info, isLoading } = useSystemInfoQuery()
  return {
    engines: info?.databases.engines ?? [],
    isLoading,
  }
}

/**
 * NoEngineImageAlert explains the one thing an operator has to do before a
 * database can be created. It names the recipe rather than a download, because
 * the image is built once, out of band, and published by whoever runs the node
 * (07-databases.md §7).
 */
export function NoEngineImageAlert() {
  return (
    <Alert type="info" header="No database engine image is available yet">
      <SpaceBetween size="xs">
        <span>
          A DB instance boots from a golden image with Postgres already installed, which is why it
          starts in about a minute instead of installing a database on first boot. Build it with the
          recipe in <Box variant="code">packaging/images/postgres-18/</Box>, publish the qcow2 where
          this node can fetch it, then{' '}
          <Link href="/images/import">import it as an engine image</Link> with engine{' '}
          <Box variant="code">postgres</Box> and version <Box variant="code">18</Box>.
        </span>
        <span>
          <Box variant="code">kumod doctor</Box> says the same thing from the command line.
        </span>
      </SpaceBetween>
    </Alert>
  )
}
