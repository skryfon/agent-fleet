import { defineConfig } from 'vitest/config'

// Root workspace config: only run tests from source, never the tsc `lib/`
// build output (both would otherwise match `*.test.ts`/`*.test.js`).
export default defineConfig({
  test: {
    exclude: ['**/lib/**', '**/node_modules/**'],
  },
})
