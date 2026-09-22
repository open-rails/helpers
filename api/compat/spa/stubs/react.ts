// Leaf stub: the upload hook's state is never read by getUploadErrorMessage.
export const useState = <T,>(init: T): [T, (v: T) => void] => [init, () => {}]
