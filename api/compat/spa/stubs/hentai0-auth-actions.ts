// Leaf stub: no token, and redirects are recorded rather than performed.
export const getToken = () => undefined
export const redirects: string[] = []
export const handleAuthFailureRedirect = (to: string) => {
  redirects.push(to)
}
