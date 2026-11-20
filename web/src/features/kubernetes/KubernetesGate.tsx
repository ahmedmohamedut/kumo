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
 * KubernetesGate renders the Kubernetes pages only on a node with the module
 * on (08-kubernetes.md §2): with it off every `/eks` route answers 503, so the
 * page says how to turn it on instead. A node with the module on and no image
 * is not gated -- that is a fresh install, and NoKubernetesImageAlert explains
 * the one import to do.
 */
export function KubernetesGate({ title, children }: { title: string; children: ReactNode }) {
  const { data: info, isLoading } = useSystemInfoQuery()

  if (isLoading) {
    return <Spinner size="large" />
  }
  if (info !== undefined && !info.kubernetes.enabled) {
    return (
      <ContentLayout header={<Header variant="h1">{title}</Header>}>
        <Alert type="info" header="Kubernetes is turned off on this node">
          Kumo runs Kubernetes clusters as virtual machines on this node. To turn the module on, set{' '}
          <Box variant="code">KUMO_KUBERNETES=on</Box> in <Box variant="code">kumod.env</Box> and
          restart kumod.
        </Alert>
      </ContentLayout>
    )
  }
  return <SpaceBetween size="m">{children}</SpaceBetween>
}

/** useKubernetesVersions is the Kubernetes minors this node has an image for. */
export function useKubernetesVersions() {
  const { data: info, isLoading } = useSystemInfoQuery()
  return { versions: info?.kubernetes.versions ?? [], isLoading }
}

/**
 * NoKubernetesImageAlert explains the import a cluster needs first. The image
 * is published by the project, gzipped, on its releases page, with the exact
 * import body in the release notes.
 */
export function NoKubernetesImageAlert() {
  return (
    <Alert type="info" header="No Kubernetes image is available yet">
      <SpaceBetween size="xs">
        <span>
          Every node of a cluster boots from an engine image with Kubernetes, CRI-O and every
          container image a cluster starts with already in it, so no node needs a registry. The
          project publishes it as a release tagged{' '}
          <Box variant="code">kubernetes-1.37-&lt;date&gt;</Box>;{' '}
          <Link href="/images/import">import it as an engine image</Link> with engine{' '}
          <Box variant="code">kubernetes</Box> and version <Box variant="code">1.37</Box>, or build
          it with <Box variant="code">packaging/images/kubernetes-1.37/</Box>.
        </span>
        <span>
          <Box variant="code">kumod doctor</Box> says the same thing from the command line.
        </span>
      </SpaceBetween>
    </Alert>
  )
}
