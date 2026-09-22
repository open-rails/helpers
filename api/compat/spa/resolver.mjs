// Maps each SPA's "@/" alias onto the vendored real module or a leaf stub, so
// probe.mjs runs the consumers' own parser files unmodified.
import { registerHooks } from 'node:module'
import { fileURLToPath, pathToFileURL } from 'node:url'
import path from 'node:path'

const here = path.dirname(fileURLToPath(import.meta.url))
const v = (p) => pathToFileURL(path.join(here, 'vendor', p)).href
const s = (p) => pathToFileURL(path.join(here, 'stubs', p)).href

const doujins = {
  '@/store': s('doujins-store.ts'),
  '@/services/auth/sdk': s('auth-sdk.ts'),
}
const hentai0 = {
  '@/hooks/auth/actions': s('hentai0-auth-actions.ts'),
  '@/services/auth/sdk': s('auth-sdk.ts'),
  '@/types/api': s('hentai0-types-api.ts'), // re-exports the real types/api/errors.ts
  '@/utils/languages': s('hentai0-languages.ts'),
  '@/utils/auth-route-guards': s('hentai0-auth-route-guards.ts'),
  axios: s('axios.ts'),
  react: s('react.ts'),
}

// The ONE transform applied to a vendored file: Vite replaces `import.meta.env`
// at build time and node cannot. Every rewrite is counted so it can never grow
// silently into a rewrite of parser logic.
globalThis.__IMPORT_META_ENV__ = {}
export const rewrites = { 'import.meta.env': 0 }

registerHooks({
  load(url, context, next) {
    const loaded = next(url, context)
    if (!url.includes('/vendor/') || loaded.source == null) return loaded
    const before =
      typeof loaded.source === 'string' ? loaded.source : Buffer.from(loaded.source).toString('utf8')
    if (!before.includes('import.meta.env')) return loaded
    rewrites['import.meta.env'] += before.split('import.meta.env').length - 1
    return { ...loaded, source: before.replaceAll('import.meta.env', 'globalThis.__IMPORT_META_ENV__') }
  },
  resolve(specifier, context, next) {
    const from = context.parentURL || ''
    const table = from.includes('/vendor/hentai0/') ? hentai0 : doujins
    if (table[specifier]) return { url: table[specifier], shortCircuit: true }
    return next(specifier, context)
  },
})
