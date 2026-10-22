/**
 * The image mirror the end-to-end suite imports from.
 *
 * The import form takes a URL and the workflow really downloads it, so the
 * suite needs something to point at. Serving a tiny generated qcow2 from
 * loopback keeps the run offline and fast: a vendor cloud image would make a
 * test that proves a form works depend on somebody else's CDN.
 *
 * kumod refuses loopback and private addresses by default as an SSRF guard, so
 * this only works because the suite starts kumod with
 * KUMO_IMAGE_ALLOW_PRIVATE_URLS=true (see scripts/e2e-serve.sh). That is the
 * documented escape hatch for a trusted LAN mirror, which is what this is.
 */
import { createServer } from 'node:http'

const PORT = Number(process.env.KUMO_E2E_FIXTURE_PORT ?? 8098)

/** HEADER_LEN is the header size the parser in internal/imagefmt reads. */
const HEADER_LEN = 104
/** BODY_LEN pads the file out so it is a plausible image rather than a header. */
const BODY_LEN = 1024

/**
 * tinyQcow2 mirrors e2e/support/qcow2.ts. It is repeated here rather than
 * imported because node runs this file directly, with no transpiler in front
 * of it.
 */
function tinyQcow2(virtualSizeBytes) {
  const header = Buffer.alloc(HEADER_LEN)

  header.write('QFI\xfb', 0, 'latin1')
  header.writeUInt32BE(3, 4)
  header.writeBigUInt64BE(0n, 8)
  header.writeUInt32BE(0, 16)
  header.writeUInt32BE(16, 20)
  header.writeBigUInt64BE(BigInt(virtualSizeBytes), 24)
  header.writeUInt32BE(2, 32)
  header.writeUInt32BE(1, 36)
  header.writeBigUInt64BE(0n, 40)
  header.writeUInt32BE(1, 48)
  header.writeUInt32BE(0, 52)
  header.writeBigUInt64BE(0n, 56)

  const body = Buffer.alloc(BODY_LEN)
  for (let i = 0; i < BODY_LEN; i++) {
    body[i] = i % 251
  }

  return Buffer.concat([header, body])
}

// One GiB of virtual size, so the launch form's root disk minimum is 1 GiB and
// a launch does not have to reserve a realistic disk to pass the capacity check.
const image = tinyQcow2(1024 * 1024 * 1024)

const server = createServer((request, response) => {
  if (request.url === '/healthz') {
    response.writeHead(200, { 'content-type': 'application/json' })
    response.end('{"status":"ok"}')
    return
  }

  if (request.url === '/tiny.qcow2') {
    // Content-Length matters: the downloader uses it for the size limit and for
    // the progress the console renders, and a chunked response would exercise
    // neither.
    response.writeHead(200, {
      'content-type': 'application/octet-stream',
      'content-length': String(image.length),
    })
    if (request.method === 'HEAD') {
      response.end()
      return
    }
    response.end(image)
    return
  }

  response.writeHead(404, { 'content-type': 'text/plain' })
  response.end('not found\n')
})

server.listen(PORT, '127.0.0.1', () => {
  console.log(`e2e image mirror on http://127.0.0.1:${PORT}/tiny.qcow2`)
})
