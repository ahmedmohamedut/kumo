import type { PresignedURL } from '@/features/buckets/api'

/**
 * putToPresignedURL sends a file straight to the object store with a
 * presigned upload URL. It is plain `fetch` on purpose: the console's API
 * client would add the Kumo access token, which the object store does not
 * know and which would break the SigV4 signature the URL already carries.
 */
export async function putToPresignedURL(signed: PresignedURL, file: Blob): Promise<void> {
  let response: Response
  try {
    response = await fetch(signed.url, {
      method: signed.method,
      headers: signed.headers,
      body: file,
    })
  } catch {
    // A CORS refusal and an unreachable server look the same from here.
    throw new Error(
      'The object storage server could not be reached from this browser. Check KUMO_S3_PUBLIC_URL and KUMO_S3_CORS_ORIGINS.',
    )
  }
  if (!response.ok) {
    throw new Error(`The object storage server refused the upload (HTTP ${response.status}).`)
  }
}

/**
 * startDownload hands a presigned download URL to the browser. The URL
 * carries Content-Disposition: attachment, so the browser saves the file and
 * the console stays where it is.
 */
export function startDownload(url: string): void {
  const link = document.createElement('a')
  link.href = url
  link.rel = 'noopener'
  document.body.appendChild(link)
  link.click()
  link.remove()
}

/** isPlainHTTP reports a console served without TLS to a non-local address,
 *  where object bytes and presigned URLs cross the network in clear. */
export function isPlainHTTP(
  location: Pick<Location, 'protocol' | 'hostname'> = window.location,
): boolean {
  return (
    location.protocol === 'http:' &&
    !['localhost', '127.0.0.1', '[::1]', '::1'].includes(location.hostname)
  )
}
