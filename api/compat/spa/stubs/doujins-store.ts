// Leaf stub: the error path never reads the auth store.
export const useAuthStore = { getState: () => ({ user: undefined }) }
