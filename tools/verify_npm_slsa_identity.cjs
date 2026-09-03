'use strict'

const crypto = require('node:crypto')
const fs = require('node:fs')
const sigstore = require('/verifier/node_modules/sigstore')
const sigstorePackage = require('/verifier/node_modules/sigstore/package.json')

const INPUT = '/input/slsa-bundle.json'
const MAXIMUM_BYTES = 4 * 1024 * 1024
const SIGSTORE_VERSION = '4.1.1'
const CERTIFICATE_IDENTITY =
  'https://github.com/lidge-jun/opencodex/.github/workflows/release.yml@refs/heads/main'
const CERTIFICATE_ISSUER = 'https://token.actions.githubusercontent.com'
// @sigstore/verify treats certificateIdentityURI as a regular expression.
// Anchors and escaped dots make this the one immutable upstream workflow SAN.
const CERTIFICATE_IDENTITY_PATTERN =
  '^https://github\\.com/lidge-jun/opencodex/\\.github/workflows/release\\.yml@refs/heads/main$'

async function main () {
  if (process.argv.length !== 3 || process.argv[2] !== INPUT) {
    throw new Error('invalid invocation')
  }
  if (sigstorePackage.version !== SIGSTORE_VERSION) {
    throw new Error('unexpected sigstore version')
  }
  const stat = fs.lstatSync(INPUT)
  if (!stat.isFile() || stat.isSymbolicLink() || stat.size <= 0 || stat.size > MAXIMUM_BYTES) {
    throw new Error('invalid bundle file')
  }
  const encoded = fs.readFileSync(INPUT)
  const bundle = JSON.parse(encoded.toString('utf8'))
  fs.mkdirSync('/tmp/tuf-identity', { mode: 0o700 })
  await sigstore.verify(bundle, {
    tufCachePath: '/tmp/tuf-identity',
    tufForceCache: true,
    certificateIdentityURI: CERTIFICATE_IDENTITY_PATTERN,
    certificateIssuer: CERTIFICATE_ISSUER
  })
  const receipt = {
    schema: 1,
    status: 'verified',
    bundle_sha256: crypto.createHash('sha256').update(encoded).digest('hex'),
    certificate_identity: CERTIFICATE_IDENTITY,
    certificate_issuer: CERTIFICATE_ISSUER,
    verifier: `sigstore@${SIGSTORE_VERSION}`
  }
  process.stdout.write(`${JSON.stringify(receipt)}\n`)
}

main().catch(() => {
  process.stderr.write('npm SLSA identity verification failed\n')
  process.exitCode = 1
})
