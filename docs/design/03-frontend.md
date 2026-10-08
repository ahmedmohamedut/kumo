# 03 — Frontend (web/)

Stack: React 18, TypeScript (strict), Vite, Redux Toolkit + RTK Query, React Router 6, Cloudscape Design System, Vitest + Testing Library + MSW.

## 1. Setup notes

- Packages: `@cloudscape-design/components`, `@cloudscape-design/global-styles`, `@cloudscape-design/collection-hooks`, `@reduxjs/toolkit`, `react-redux`, `react-router-dom`.
- Import `@cloudscape-design/global-styles/index.css` once in `main.tsx`. Support light/dark via `applyMode` from `@cloudscape-design/global-styles` (persist preference in `localStorage`).
- Vite dev proxy: `/api` → `http://localhost:8080`. Production build output `web/dist` is embedded by Go.
- API types generated from `api/openapi.yaml` (`openapi-typescript`) into `src/types/api.ts`.
- Path alias `@/` → `src/`.

## 2. Layout (AWS-console style)

```
┌───────────────────────────────────────────────────────────────────────────┐
│ TopNavigation: [☁ Kumo]  [Services ▾]      [search]   [node: kumo-01] [user ▾]│  ← sticky header (#h)
├───────────┬───────────────────────────────────────────────────────────────┤
│ SideNav   │ BreadcrumbGroup                                                │
│           │ Flashbar (notifications)                                       │
│ Compute   │ ┌ ContentLayout ───────────────────────────────────────────┐  │
│  Instances│ │ Header (title, counter, actions)                         │  │
│  Images   │ │ Table / Cards / Detail                                    │  │
│  Key pairs│ └──────────────────────────────────────────────────────────┘  │
│ (future:  │                                                                │
│  Storage, │                                                                │
│  Database)│                                                                │
└───────────┴───────────────────────────────────────────────────────────────┘
```

- `AppLayout` (navigation = `SideNavigation`, breadcrumbs, notifications = `Flashbar`, `toolsHide` for now, `headerSelector="#h"`, content type per page: `table`, `form`, `wizard`, `default`).
- `TopNavigation`: logo + "Kumo", a **Services** utility menu listing available service groups (Compute active; others disabled with "Coming soon" badges), user menu (profile, theme toggle, sign out).
- Side navigation items: **Dashboard**, **Compute** (section: Instances, Images, Key pairs). Active item follows the route.
- Layout component: `components/layout/ConsoleLayout.tsx`; routes render inside via `<Outlet />`. Pages set breadcrumbs through a small context/hook (`usePageMeta({breadcrumbs})`).

## 3. Routes

| Path | Page | Notes |
|---|---|---|
| `/login` | LoginPage | Public. Redirect to `?next=` after login |
| `/setup` | SetupPage | Only when `needs_setup` |
| `/` | Dashboard | Counts of instances by state, recent tasks, capacity bars |
| `/instances` | InstancesList | Table, actions, filters |
| `/instances/launch` | LaunchWizard | Single-page form (v1), `Wizard` later |
| `/instances/:id` | InstanceDetail | Tabs: Details, Storage, Tags, Console output |
| `/images` | ImagesList | Table + Import button |
| `/images/import` | ImportImage | Form with catalog picker |
| `/images/:id` | ImageDetail | Details, tags, activity (tasks), retry/delete |
| `/key-pairs` | KeyPairsList | Table + Create/Import |
| `/key-pairs/create` | CreateKeyPair | Shows private key once in a modal with Download .pem + Copy |
| `/key-pairs/import` | ImportKeyPair | |
| `*` | NotFound | |

Protected routes wrapped by `RequireAuth`.

## 4. Auth flow

- Redux `auth` slice: `{ user, accessToken (memory only), status: 'unknown'|'authenticated'|'anonymous' }`.
- On app start, call `POST /auth/refresh` (cookie) → if OK, load `/auth/me`; else `anonymous`.
- RTK Query `baseQuery` wrapper: attaches `Authorization`; on `401` runs a **single-flight** refresh then retries once; if refresh fails, dispatch `loggedOut` and redirect to `/login?next=…`.
- Login page: Cloudscape `Container` centered on a plain background, `Form` with `FormField`s (`Input`, `Input type=password`), inline error `Alert` on failure, `Button variant="primary"` with loading state. No "forgot password" in v1.

## 5. Feature module structure

```
src/features/instances/
  api.ts            // RTK Query endpoints: list, get, launch, start, stop, reboot, terminate
  InstancesList.tsx
  InstanceDetail.tsx
  LaunchForm.tsx
  columns.tsx       // table column definitions
  status.ts         // state → StatusIndicator mapping
src/features/images/…
src/features/keypairs/…
src/features/auth/…
src/features/notifications/   // flashbar slice + hooks
```

RTK Query tags: `Instance`, `Image`, `KeyPair`, `Task`. Mutations invalidate lists. Pages with transitional resources use `pollingInterval: 5000`, otherwise 30000.

The interval follows the data, not the route: the images and tasks pages pass `pollInterval(items)` to the hook, which returns 5000 while anything is `pending`/`downloading`/`deleting` (images) or `pending`/`running` (tasks) and 30000 once everything has settled. RTK Query types `pollingInterval` as a constant on the endpoint, so the value has to come from the cached copy of the query — read through `endpoints.<name>.select(args)` in a `useAppSelector` — and be passed as a per-hook option. An operator who leaves the tab open during an import watches it arrive, and does not then pay for a 5 s loop over a settled table forever.

## 6. Page specs

### 6.1 Instances list
- `Table` with `useCollection` (filtering, sorting, pagination 20/50/100, selection `single`), `CollectionPreferences` (visible columns, page size, wrap lines).
- Columns: **Name**, **Instance ID** (link), **State**, **Instance type**, **Private IP**, **Image**, **Key pair**, **Launched**.
- Header: title "Instances" with counter `(n)`; `ButtonDropdown` **Instance state** (Start, Stop, Force stop, Reboot, Terminate — enabled by selection state); primary `Button` **Launch instance**; refresh `Button iconName="refresh"`.
- Empty state: "No instances" + Launch button.
- Terminate/force-stop → confirmation `Modal` (type instance name for terminate).

State → `StatusIndicator`:

| State | Type | Text |
|---|---|---|
| pending | `pending` | Pending |
| running | `success` | Running |
| stopping | `in-progress` | Stopping |
| stopped | `stopped` | Stopped |
| terminating | `in-progress` | Terminating |
| terminated | `stopped` | Terminated |
| error | `error` | Error (with `state_reason` underneath) |

The reason is rendered as small inactive text *below* the indicator, not as the indicator's tooltip. A tooltip is only reachable on hover, which hides the one thing an operator scanning a column of failures is looking for; a `pending` row says what it is waiting for on the same grounds.

### 6.2 Launch instance (form)
Sections (`SpaceBetween` of `Container`s, sticky footer actions Cancel / **Launch instance**):
1. **Name and tags**: name `Input`, tags editor (`TagEditor`).
2. **Image**: `Select` (or `Cards` with `selectionType="single"`) of `available` images, shows OS, version, min disk.
3. **Instance type**: `Select` showing `vcpus / memory`.
4. **Key pair**: `Select` + link to "Create new key pair" (opens in new tab) + option "Proceed without a key pair".
5. **Storage**: root disk size `Input type=number` (GiB), min from image.
6. **Advanced details**: `Textarea` for user data (cloud-init), with hint.
7. **Summary** side panel: selected values.

On submit → `POST /instances` → navigate to the detail page and show a success Flashbar item "Instance i-… is being launched".

### 6.3 Instance detail
`ContentLayout` header with name, ID, state indicator and actions. `Tabs`: **Details** (`ColumnLayout` of `KeyValuePairs`: ID, state, type, vCPU, memory, image, key pair, IPs, launch time, MAC), **Storage** (root disk), **Tags**, **Console output** (`<pre>` with Refresh; VNC console button added in M5), **Tasks** (recent tasks).

The lifecycle actions are here as well as on the list. An operator who followed a launch to this page is watching an instance come up, and the thing they want when a boot hangs is Stop — not three clicks and a page change away. The guard table and the confirmations are shared with the list, so the two cannot disagree.

`user_data` is not shown. The endpoint only carries it for an administrator (01-api.md §Instances) and the launch form is the only place it is typed; the detail page has nothing useful to add, and printing a secret-adjacent payload into a page an operator might screenshot is not a trade worth making.

### 6.4 Images
List columns: Name, Image ID, OS, Architecture, State, Size, Min disk, Created. The OS column shows `os_family os_version`, falling back to the source URL's host when an import carried no OS metadata. Filtering is server-side (`?q=`) with the filter kept in the URL; sorting and paging run over the fetched page (`page_size` is the API maximum, so one request covers a whole node).

The State cell carries the whole state, not just its name: `state_reason` under a failed or pending indicator, and a `ProgressBar` while a download is under way. The bar appears only once a percentage exists — a chunked download has no `Content-Length`, and a bar frozen at 0% for the length of a 600 MB transfer reads as "nothing is happening" when something very much is.

Actions: Import image, Delete (confirmation modal naming the image; a refused delete stays on the page and shows the `409`, which names the instances that block it). Import and Delete answer `202`, so both leave the operator on a page that follows the task: the import form goes to the new image, the delete returns to the list where the row shows `deleting` until the task finishes.

Detail page (`ContentLayout`): state indicator, Details (`Grid` of label/value rows: ID, architecture, OS, default user, format, size, min disk, source URL, checksum, timestamps), Tags, and Activity — the tasks for that image with their state and error. Retry import appears only for a failed image; a retry queues a **new** task rather than resurrecting the old one.

Import form: pick from **catalog** or paste a URL, plus algorithm/checksum, OS metadata, default user and min disk. Choosing a catalog entry fills in the URL, the OS, the default user and the min disk from what the node already knows; the **name** comes from the catalog `id` and not its `name`, because a picker label ("Debian 12 (bookworm)") is not a legal image name. A catalog entry carries a `checksum_url`, a link to the vendor's `SHA256SUMS` — not a digest the API can verify on its own, so the checksum field stays empty until the operator pastes the hash.

### 6.5 Key pairs
List columns: Name, ID, Type, Fingerprint, Created. Actions: Create key pair, Import, Delete.
Create form: name, type (ED25519 default / RSA), tags. On success show a **Modal** "Save your private key": download as `<name>.pem` (`.pem` with `0600` reminder text) and Copy; warn it can't be shown again; closing requires an explicit "I have saved it" checkbox.

## 7. UX details

- **Notifications:** `notifications` slice holds Flashbar items; each mutation result pushes a success/error item (dismissible, every notification auto-dismisses after 5 s via `features/notifications/useAutoDismiss.ts`).
- **Errors:** map API `details[].field` onto `FormField errorText`.
- **Loading:** `Table loading`, `Spinner`, skeleton states via `loadingText`.
- **Time:** relative + absolute tooltip (`lib/format.ts`). **Bytes/GiB** formatters in `lib/format.ts`.
- **i18n:** English only; keep strings in components for now but route `i18nStrings` through a single `lib/i18nStrings.ts`.
- **Responsive:** rely on Cloudscape's responsive layout; verify at 1280 and 1920 widths.

## 8. Testing

- Unit: formatters, status mapping, auth slice, baseQuery refresh logic.
- Component: lists render, selection enables actions, launch form validation, private-key modal flow.
- Network mocked with MSW; handlers in `src/mocks/handlers.ts` mirror `openapi.yaml`.
- E2E (Playwright, later M6): login → import image → create key pair → launch → stop → terminate against `KUMO_HYPERVISOR=fake`.
