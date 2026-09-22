// Leaf stub. `isAxiosError` mirrors axios' own implementation (it checks
// `payload.isAxiosError === true`); `post` throws the axios-shaped error the
// probe planted, which is how the upload hook's real error path is reached.
export const isAxiosError = (e: any) => !!e && e.isAxiosError === true
export const nextFailure: { error: any } = { error: null }
const fail = async () => {
  throw nextFailure.error
}
const axios: any = { post: fail, put: fail }
export default axios
