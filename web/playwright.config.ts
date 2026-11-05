import { defineConfig, devices } from '@playwright/test'

/**
 * End-to-end configuration: a browser against the real binary.
 *
 * The suite drives the console that `make build` embeds into kumod, talking to
 * the real API, the real SQLite database and the real task worker, with the
 * in-memory hypervisor standing in for KVM. That is the whole point of the
 * `fake` backend — everything above libvirt is exercised exactly as it ships,
 * on a machine with no /dev/kvm (which includes every hosted runner).
 *
 * The component tests in `src/**` cover the UI against mocked network calls.
 * These cover what those cannot: that the console and the backend agree.
 */

const consolePort = Number(process.env.KUMO_E2E_PORT ?? 8099)
const fixturePort = Number(process.env.KUMO_E2E_FIXTURE_PORT ?? 8098)
const consoleURL = `http://127.0.0.1:${consolePort}`

/**
 * The range the suite's default VPC is created with, pinned away from Kumo's
 * own default (192.168.100.0/24) because that is a common home-router range:
 * the bootstrap refuses a default VPC that would shadow a host route, and the
 * node then comes up with no default subnet and refuses every launch. The
 * specs assert addresses from this range, so it is defined once here and read
 * by both the server script and the tests.
 */
const vpcCIDR = process.env.KUMO_E2E_VPC_CIDR ?? '10.111.0.0/24'

export default defineConfig({
  testDir: './e2e',

  /**
   * One worker, in order. The suite shares one backend and one database, and
   * the journey is cumulative: a key pair is created before an instance can
   * use it, and an image has to be imported before anything can boot from it.
   * Parallel workers would race each other through the same state.
   *
   * It also means no retries: re-running a test that has already mutated the
   * database would start from state its first attempt left behind, and a green
   * second attempt would mean nothing.
   */
  fullyParallel: false,
  workers: 1,
  retries: 0,

  // `test.only` left in a file would quietly shrink the suite to one case.
  forbidOnly: !!process.env.CI,

  timeout: 60_000,
  expect: {
    // Generous because the console polls rather than subscribing: a transitional
    // state is picked up on the next 5 s poll, so an assertion about a guest
    // becoming `running` has to outlast one poll interval (03-frontend.md).
    timeout: 15_000,
  },

  reporter: process.env.CI
    ? [['github'], ['html', { open: 'never' }], ['list']]
    : [['list'], ['html', { open: 'never' }]],

  use: {
    baseURL: consoleURL,
    // Kept only for failures: a trace per test would be most of a run's time
    // and all of its disk.
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],

  webServer: [
    {
      // The loopback mirror the image import downloads from.
      command: 'node e2e/support/image-server.mjs',
      url: `http://127.0.0.1:${fixturePort}/healthz`,
      env: { KUMO_E2E_FIXTURE_PORT: String(fixturePort) },
      reuseExistingServer: !process.env.CI,
      stdout: 'ignore',
      stderr: 'pipe',
    },
    {
      command: 'bash ../scripts/e2e-serve.sh',
      url: `${consoleURL}/healthz`,
      env: { KUMO_E2E_PORT: String(consolePort), KUMO_E2E_VPC_CIDR: vpcCIDR },
      /**
       * Never reused, unlike the mirror. The suite starts at the first-run
       * screen, which only exists while no user has been created, so a kumod
       * somebody left running — with a database and an administrator already in
       * it — would fail the first test for a reason that has nothing to do with
       * the code. A fresh process wipes its data directory on start.
       */
      reuseExistingServer: false,
      stdout: 'ignore',
      stderr: 'pipe',
      timeout: 60_000,
    },
  ],
})
