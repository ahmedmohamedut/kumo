import type { ReactNode } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Header from '@cloudscape-design/components/header'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'

import { useSystemInfoQuery } from '@/features/auth/api'

/**
 * ObjectStorageGate renders the Storage pages only on a node with object
 * storage on (06-object-storage.md §2). With the module off, every /s3 route
 * answers 503, so the page explains how to turn it on instead of rendering a
 * table that can only fail. A server that is configured but not answering
 * keeps the page, under a warning, because the next request may succeed.
 */
export function ObjectStorageGate({ title, children }: { title: string; children: ReactNode }) {
  const { data: info, isLoading } = useSystemInfoQuery()

  if (isLoading) {
    return <Spinner size="large" />
  }

  if (info?.object_storage.backend === 'none') {
    return (
      <ContentLayout header={<Header variant="h1">{title}</Header>}>
        <Alert type="info" header="Object storage is not configured on this node">
          <SpaceBetween size="xs">
            <span>
              Kumo manages buckets on an S3-compatible RustFS server you run. To turn it on, set{' '}
              <Box variant="code">KUMO_OBJECTSTORE=rustfs</Box> and{' '}
              <Box variant="code">KUMO_S3_ENDPOINT</Box> in <Box variant="code">kumod.env</Box>, put
              the RustFS keys in the credentials file, and restart kumod.
            </span>
            <span>
              <Box variant="code">kumod doctor</Box> checks the connection.
            </span>
          </SpaceBetween>
        </Alert>
      </ContentLayout>
    )
  }

  return (
    <SpaceBetween size="m">
      {info !== undefined && !info.object_storage.ok && (
        <Alert type="warning" header="The object storage server is not answering">
          Kumo cannot reach the RustFS server with its keys right now. Buckets cannot be listed or
          changed until it answers; <Box variant="code">kumod doctor</Box> shows why.
        </Alert>
      )}
      {children}
    </SpaceBetween>
  )
}
