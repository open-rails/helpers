// api-service.ts imports `APIError` with a value import although it is an
// interface. The SPA's bundler erases it; node's type-stripper cannot, so this
// re-exports the REAL module (getUserErrorMessage and all) and adds the one
// binding that would otherwise have been erased.
export * from '../vendor/hentai0/types/api/errors.ts'
export const APIError = undefined
