/**
 * Saving a generated private key as a file.
 *
 * These live apart from the dialog component because a module that exports both
 * components and helpers breaks React Fast Refresh, and because they are worth
 * testing without rendering anything.
 */

/**
 * pemFileName is what a downloaded private key is called. The extension is `.pem`
 * because that is what an operator's tooling expects, but an OpenSSH private key
 * in PEM armour is still a private key: the name says so loudly enough that nobody
 * commits one by accident.
 */
export function pemFileName(name: string): string {
  return `${name}.pem`
}

/**
 * downloadPrivateKey saves the PEM through a blob URL.
 *
 * A download cannot be done with a plain `href` because the key only ever exists
 * in memory, so the click has to be synthesised. The object URL is revoked right
 * after: leaving it alive keeps the private key in memory for the lifetime of the
 * document, which is exactly what the save dialog exists to avoid.
 */
export function downloadPrivateKey(name: string, privateKey: string): void {
  const blob = new Blob([privateKey], { type: 'application/x-pem-file' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = pemFileName(name)
  document.body.append(anchor)
  anchor.click()
  anchor.remove()
  URL.revokeObjectURL(url)
}
