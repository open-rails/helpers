// Feeds every golden writer body to the consumers' REAL parsers over a real
// socket and records what each one produced. This is the evidence for any wire
// change: if a field disappears, the answer is in golden/parsers.json, not in
// an argument about what parsers probably do.
//
//   node probe.mjs            verify golden/parsers.json
//   node probe.mjs --update   rewrite it
import './resolver.mjs'
import http from 'node:http'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const goldenDir = path.join(here, '..', 'golden')
const out = path.join(here, 'golden', 'parsers.json')

// --- the fixtures -----------------------------------------------------------

function readFixtures(dir, prefix = '') {
  const found = []
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    if (e.isDirectory()) found.push(...readFixtures(path.join(dir, e.name), prefix + e.name + '/'))
    else if (e.name.endsWith('.http')) {
      const raw = fs.readFileSync(path.join(dir, e.name), 'utf8')
      const split = raw.indexOf('\n\n')
      const head = raw.slice(0, split).split('\n')
      const body = raw.slice(split + 2)
      const status = Number(head[0].split(' ')[1])
      const headers = {}
      for (const line of head.slice(1)) {
        const i = line.indexOf(': ')
        headers[line.slice(0, i)] = line.slice(i + 2)
      }
      found.push({ name: prefix + e.name.replace(/\.http$/, ''), status, headers, body })
    }
  }
  return found.sort((a, b) => a.name.localeCompare(b.name))
}

// --- a real server ----------------------------------------------------------

let current = null
const server = http.createServer((req, res) => {
  res.writeHead(current.status, current.headers)
  res.end(current.body)
})
await new Promise((r) => server.listen(0, '127.0.0.1', r))
const base = `http://127.0.0.1:${server.address().port}`

globalThis.window = { lang: 'en', location: { pathname: '/', href: base } }

// --- the parsers ------------------------------------------------------------

const { createHttpClient } = await import('./vendor/doujins/lib/http/client.ts')
const { ApiService } = await import('./vendor/hentai0/services/api/api-service.ts')
const { useUploadVideo } = await import('./vendor/hentai0/hooks/upload/useUploadVideo.ts')
const { nextFailure } = await import('./stubs/axios.ts')

const doujinsClient = createHttpClient({ baseUrl: base, auth: true })
const hentai0Client = new ApiService(base, '')
const upload_ = useUploadVideo()

const plain = (v) => JSON.parse(JSON.stringify(v, (_k, x) => (x instanceof Error ? { name: x.name, message: x.message, status: x.status, data: x.data, body: x.body } : x)))

async function doujins(f) {
  current = f
  try {
    const value = await doujinsClient.get('/probe')
    return { outcome: 'resolved', value: plain(value) }
  } catch (e) {
    return {
      outcome: 'threw',
      // What the UI shows, and what authErrorLocalization.ts keys off.
      displayedMessage: e.message,
      localizationKey: e?.data?.error ?? null,
      metadataSpreadOnto: e?.data ? Object.keys(e.data).filter((k) => k !== 'error' && k !== 'object') : [],
    }
  }
}

async function hentai0(f, status) {
  current = { ...f, status: status ?? f.status }
  try {
    const value = await hentai0Client.get('/probe')
    return { outcome: 'resolved', value: plain(value) }
  } catch (e) {
    return {
      outcome: 'threw',
      userMessage: e?.userMessage ?? null,
      type: e?.type ?? null,
      code: e?.code ?? null,
      branch: e?.type !== undefined ? 'stripe-envelope' : 'house-format',
    }
  }
}

// Reaches getUploadErrorMessage the way the UI does: presignUpload rejects
// with an axios error carrying the writer's body, and uploadQueue reports it.
async function upload(f) {
  let data
  try {
    data = JSON.parse(f.body)
  } catch {
    data = f.body
  }
  nextFailure.error = { isAxiosError: true, response: { status: f.status, data } }
  let shown = null
  await upload_.uploadQueue([{ id: 'item-1', file: new File(['x'], 'v.mp4') }], {
    onItemError: (_id, message) => {
      shown = message
    },
  })
  return { shownToTheUploader: shown }
}

// --- run --------------------------------------------------------------------

const fixtures = readFixtures(goldenDir)
const results = {}
for (const f of fixtures) {
  results[f.name] = {
    'doujins/client.ts': await doujins(f),
    'hentai0/api-service.ts': await hentai0(f),
    // The one branch that reads `object`: an error body delivered with 200.
    'hentai0/api-service.ts@200': await hentai0(f, 200),
    'hentai0/useUploadVideo.ts': await upload(f),
  }
}
server.close()

const text = JSON.stringify({ fixtures: fixtures.length, results }, null, 2) + '\n'
if (process.argv.includes('--update')) {
  fs.mkdirSync(path.dirname(out), { recursive: true })
  fs.writeFileSync(out, text)
  console.log(`wrote ${out} (${fixtures.length} fixtures)`)
} else {
  const want = fs.readFileSync(out, 'utf8')
  if (want !== text) {
    console.error('SPA parser behaviour drifted from golden/parsers.json')
    process.exit(1)
  }
  console.log(`SPA parser fixtures match (${fixtures.length} fixtures)`)
}
