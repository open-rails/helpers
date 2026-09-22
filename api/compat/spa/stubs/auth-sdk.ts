// Leaf stub: no 401 refresh is exercised; every probe body is a terminal error.
export const authSDK = {
  getAccessToken: () => undefined,
  refreshTokens: async () => false,
  getTokenManager: () => ({ getGeneration: () => 1 }),
}
