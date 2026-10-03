import { applyMode, Mode } from '@cloudscape-design/global-styles'

const storageKey = 'kumo.colorMode'

/** Reads the stored colour mode, defaulting to the light theme. */
export function storedMode(): Mode {
  try {
    return window.localStorage.getItem(storageKey) === Mode.Dark ? Mode.Dark : Mode.Light
  } catch {
    // Private browsing modes can throw on access; the default theme is fine.
    return Mode.Light
  }
}

/** Applies the stored colour mode to the document. */
export function applyStoredMode(): Mode {
  const mode = storedMode()
  applyMode(mode)
  return mode
}

/** Flips between light and dark and remembers the choice. */
export function toggleMode(): Mode {
  const next = storedMode() === Mode.Dark ? Mode.Light : Mode.Dark
  applyMode(next)
  try {
    window.localStorage.setItem(storageKey, next)
  } catch {
    // Not being able to persist the preference is not worth failing over.
  }
  return next
}
