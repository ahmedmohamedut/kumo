/**
 * A structurally valid, tiny qcow2 image.
 *
 * The import workflow parses the header and refuses anything that is not a real
 * image, so the suite cannot hand it an arbitrary blob — and it must not reach
 * for a vendor cloud image either, because an end-to-end run has no business
 * downloading half a gigabyte from the internet to prove a form works.
 *
 * This is the TypeScript twin of `internal/imagefmt/qcow2test`, kept in the same
 * shape so the two cannot drift on what "a tiny qcow2" means. If the parser in
 * `internal/imagefmt` grows stricter, both have to move.
 */

/** HEADER_LEN is the header size the parser in internal/imagefmt reads. */
const HEADER_LEN = 104

/** BODY_LEN pads the file out so it is a plausible image rather than a header. */
const BODY_LEN = 1024

/**
 * tinyQcow2 returns a qcow2 file declaring the given virtual size in bytes.
 */
export function tinyQcow2(virtualSizeBytes: number): Buffer {
  const header = Buffer.alloc(HEADER_LEN)

  header.write('QFI\xfb', 0, 'latin1') // magic
  header.writeUInt32BE(3, 4) // version 3
  header.writeBigUInt64BE(0n, 8) // no backing file
  header.writeUInt32BE(0, 16) // mtime
  header.writeUInt32BE(16, 20) // 2^16 = 64 KiB clusters
  header.writeBigUInt64BE(BigInt(virtualSizeBytes), 24)
  header.writeUInt32BE(2, 32) // l1 size
  header.writeUInt32BE(1, 36) // l1 table offset
  header.writeBigUInt64BE(0n, 40) // refcount table
  header.writeUInt32BE(1, 48) // refcount table clusters
  header.writeUInt32BE(0, 52) // nb snapshots
  header.writeBigUInt64BE(0n, 56) // snapshots offset

  const body = Buffer.alloc(BODY_LEN)
  for (let i = 0; i < BODY_LEN; i++) {
    body[i] = i % 251
  }

  return Buffer.concat([header, body])
}
