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
 * SparkGate renders the Spark pages only on a node with the module on
 * (09-spark.md section 2): with it off every `/spark` route answers 503, so
 * the page says how to turn it on instead. No image is not gated -- that is a
 * fresh install, and NoSparkImageAlert explains the import.
 */
export function SparkGate({ title, children }: { title: string; children: ReactNode }) {
  const { data: info, isLoading } = useSystemInfoQuery()

  if (isLoading) {
    return <Spinner size="large" />
  }
  if (info !== undefined && !info.spark.enabled) {
    return (
      <ContentLayout header={<Header variant="h1">{title}</Header>}>
        <Alert type="info" header="Spark is turned off on this node">
          Kumo runs Spark clusters as virtual machines on this node. To turn the module on, set{' '}
          <Box variant="code">KUMO_SPARK=on</Box> in <Box variant="code">kumod.env</Box> and restart
          kumod.
        </Alert>
      </ContentLayout>
    )
  }
  return <SpaceBetween size="m">{children}</SpaceBetween>
}

/** useSpark is the Spark minors this node has an image for, and whether a
 * cluster can be given object storage here. */
export function useSpark() {
  const { data: info, isLoading } = useSystemInfoQuery()
  return {
    versions: info?.spark.versions ?? [],
    objectStorage: info?.spark.object_storage ?? false,
    isLoading,
  }
}

/** NoSparkImageAlert explains the import a cluster needs first. */
export function NoSparkImageAlert() {
  return (
    <Alert type="info" header="No Spark image is available yet">
      <SpaceBetween size="xs">
        <span>
          Every node of a Spark cluster boots from an engine image with Java, Spark, PySpark and the
          S3 connector already in it. The project publishes it as a release tagged{' '}
          <Box variant="code">spark-4.3-&lt;date&gt;</Box>;{' '}
          <Link href="/images/import">import it as an engine image</Link> with engine{' '}
          <Box variant="code">spark</Box> and version <Box variant="code">4.3</Box>, or build it
          with <Box variant="code">packaging/images/spark-4.3/</Box>.
        </span>
        <span>
          <Box variant="code">kumod doctor</Box> says the same thing from the command line.
        </span>
      </SpaceBetween>
    </Alert>
  )
}
