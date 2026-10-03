import { setupServer } from 'msw/node'

import { handlers } from './handlers'

/**
 * The MSW server every test runs against. `src/test/setup.ts` starts it before
 * each test and resets the handlers afterwards, so a test that overrides an
 * endpoint cannot leak that override into the next one.
 */
export const server = setupServer(...handlers)
