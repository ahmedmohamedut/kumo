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
- `TopNavigation`: logo + "Kumo", a **Services** utility menu listing the service groups, each opening its first page. Storage is disabled and marked "Not configured" on a node without object storage. User menu: profile, theme toggle, sign out.
- Side navigation items: **Dashboard**, **Compute** (Instances, Instance types, Images, Key pairs), **Networking** (VPCs, Subnets), **Storage** (Buckets, Access keys), **Databases**, **Kubernetes** (Clusters) and **Spark** (Clusters). Storage appears only when `/system/info` reports `object_storage.backend` other than `none`, Databases only when it reports `databases.enabled`, Kubernetes only when it reports `kubernetes.enabled`, and Spark only when it reports `spark.enabled` (`navigationFor`). An info payload that has not arrived yet keeps Databases, Kubernetes and Spark: the modules are on by default, so hiding them while loading would flicker them away on most nodes. Active item follows the route.
- Layout component: `components/layout/ConsoleLayout.tsx`; routes render inside via `<Outlet />`. Pages set breadcrumbs through a small context/hook (`usePageMeta({breadcrumbs})`).

## 3. Routes

| Path | Page | Notes |
|---|---|---|
| `/login` | LoginPage | Public. Redirect to `?next=` after login, or to `/setup` while `needs_setup` |
| `/setup` | SetupPage | Only when `needs_setup`; redirects to `/login` once the node has a user |
| `/` | Dashboard | Counts of instances by state, recent tasks, capacity bars |
| `/instances` | InstancesList | Table, actions, filters |
| `/instances/launch` | LaunchWizard | Single-page form (v1), `Wizard` later |
| `/instances/:id` | InstanceDetail | Tabs: Details, Storage, Tags, Console output |
| `/instance-types` | InstanceTypesList | Table + Create/Edit/Delete (admin) |
| `/instance-types/create` | CreateInstanceType | |
| `/instance-types/:id/edit` | EditInstanceType | Name read only |
| `/images` | ImagesList | Table + Import button |
| `/images/import` | ImportImage | Form with catalog picker |
| `/images/:id` | ImageDetail | Details, tags, activity (tasks), retry/delete |
| `/key-pairs` | KeyPairsList | Table + Create/Import |
| `/key-pairs/create` | CreateKeyPair | Shows private key once in a modal with Download .pem + Copy |
| `/key-pairs/import` | ImportKeyPair | |
| `/buckets` | BucketsList | Table + Create/Delete (admin); behind the object storage gate |
| `/buckets/create` | CreateBucket | Name, versioning, tags |
| `/buckets/:name` | BucketDetail | Tabs: Objects (browser), Properties |
| `/access-keys` | AccessKeysList | Table + Create/Edit/Delete (admin) |
| `/access-keys/create` | CreateAccessKey | Shows the secret once in a modal |
| `/access-keys/:id/edit` | EditAccessKey | Name, access, buckets; the secret is unchanged |
| `/databases` | DBInstancesList | Table + Create/Actions (admin); behind the databases gate |
| `/databases/create` | CreateDBInstance | Engine, identifier, credentials, size and storage |
| `/databases/:id` | DBInstanceDetail | Details, Connect panel, activity |
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

RTK Query tags: `Instance`, `InstanceType`, `Image`, `KeyPair`, `Task`, `VPC`, `Subnet`, `Bucket`, `BucketObjects`, `AccessKey`, `DBInstance`. Mutations invalidate lists. A DB instance mutation invalidates `Instance` as well: the hidden server behind it holds the node's vCPUs, memory and disk that `/system/capacity` reports. Pages with transitional resources use `pollingInterval: 5000`, otherwise 30000.

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

`terminated` stays in the mapping for the rows an earlier release left behind — a terminate deletes the row now, so nothing the console does produces the state — and for the seeded fixtures.

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

A terminate leaves this page rather than waiting on it: the task deletes the row, so the console goes back to the list the moment the call is accepted and the success Flashbar says what happened there. A page left open against an instance somebody else removed reads `404`, which is an `info` alert ("This instance no longer exists") rather than an error one: the API is answering a question, not reporting a fault.

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

### 6.6 Instance types
List columns: Name (the id, e.g. `kumo.small`), vCPUs, Memory (whole GiB when it divides evenly, MiB otherwise — `formatMemoryMiB`), Description. The API order (smallest first) is the default; columns sort on top of it. Single selection; header actions Refresh, Edit, Delete, **Create instance type**. There is no detail page: everything a type has fits in a row.

The write actions are disabled for a viewer (`selectIsAdmin`) and the row selection is hidden, rather than offering buttons that can only answer `403`; the create/edit form shows a "Read only" alert and a disabled submit for the same reason.

Delete opens a `Modal` that first reads `GET /instance-types/{id}`. While `used_by` is non-empty it names those instances (links to their detail pages) and the Delete button stays disabled, because the API refuses the delete with `409` (any instance row blocks it, terminated ones included).

Create/edit is one form (`InstanceTypeFormPage`): Name (read only on edit — instances reference it), vCPUs, Memory (MiB, with the formatted size under the label), optional Description ("generated from the size when empty"). Bounds are checked locally before a request. An edit sends **only the fields that changed**: an untouched description is left out so the API can regenerate a generated one for the new size; an edit with no change shows "Nothing to save" instead of sending an empty `PATCH`. The `InstanceType` tag is shared with the launch form's picker, so an edit there is offered on the next launch without a reload.

### 6.7 Buckets (Storage)
Every Storage page renders inside `ObjectStorageGate` (`features/buckets`). It reads `/system/info`:
- With `object_storage.backend: "none"`, the page is an info alert explaining how to turn the module on (`KUMO_OBJECTSTORE`, `KUMO_S3_ENDPOINT`, the credentials file, `kumod doctor`) instead of a table whose every request answers `503`.
- With `ok: false`, the page still renders, under a warning that the server is not answering.

**List.**
- Columns: Name (link), State (`available` → success "Available", `deleting` → in-progress "Deleting"), Created by ("Outside Kumo" for a bucket made with another S3 client), Created, Tags (a count).
- There is no versioning column: the list endpoint does not carry it, and fetching it would cost one request per bucket.
- Single selection, admin only. A `deleting` row cannot be selected. Header actions: Refresh, Delete, **Create bucket**.
- The list polls at 5 s only while a bucket is `deleting`. Otherwise it does not poll: each request lists the object store, and nothing else about a bucket changes on its own.

**Delete** is one `DeleteBucketModal`, shared by the list and the detail page:
- A plain delete answers `204` for an empty bucket.
- On `409`, the dialog says the bucket is not empty, including the case of only old versions, and points to **Delete all contents**.
- That checkbox switches to the forced delete (`?force=true` → `202` and a `bucket.delete` task). Because it cannot be undone, the confirm button stays disabled until the bucket name is typed exactly.

**Create.**
- Fields: name (checked locally against the S3 rule first), a **versioning** toggle and tags.
- The API answers `201` with the bucket ready, so the page opens its detail page.

**Detail** has two tabs, **Objects** (the default) and **Properties**.

**Objects** is `ObjectBrowser`, one folder at a time like the S3 console:
- The folder is in the URL (`?prefix=`), so links and the back button work. A breadcrumb of the folder path sits above the table.
- Rows are the folders (common prefixes, opened by clicking) and then the objects, with name, type, size and last modified.
- Paging uses the API cursor through an open-ended `Pagination`.
- **Upload** takes several files. For each, it asks for a presigned `PUT` and sends the file with plain `fetch` (`transfer.ts`), never the API client: the Kumo token would break the SigV4 signature, and the bytes must not pass through kumod.
- A per-file status list shows uploading, done or the reason for a failure. A network error is reported as unreachable or refused by CORS, naming `KUMO_S3_PUBLIC_URL` and `KUMO_S3_CORS_ORIGINS`. The folder is listed again when the files settle.
- **Download** asks for a presigned `GET` and hands it to the browser through a temporary link. The URL carries `Content-Disposition: attachment`, so the console stays put.
- **Create folder** is a modal for a single name, with no slashes.
- **Delete** confirms the selected keys. It explains that a folder delete removes only the folder marker, and that a versioned bucket keeps the previous version.
- A viewer can browse and download; upload, folder and delete are disabled for a viewer and while the bucket is `deleting`.
- When the console is served over plain HTTP to a non-local address (`isPlainHTTP`), a warning says objects and their links cross the network unencrypted.

**Properties:**
- **Details**: state, created, creator, versioning. Versioning is an inline Enable/Suspend button rather than a field, because S3 cannot return a bucket to "never versioned"; the button says what it will do.
- **Connect**: endpoint, region and bucket name, plus a copyable `aws --endpoint-url … s3 ls s3://<name>/` example.
- **Tags** editor, and **Activity**: tasks under `bucket/<name>`.
- Writes are disabled for a viewer and while the bucket is `deleting`.
- A bucket that is gone (`404`), whether deleted here or by another client, gets an info alert with a way back.

### 6.8 Access keys (Storage)
Behind `ObjectStorageGate`, like the bucket pages.

**List.**
- Columns: name, access key id, access ("Read only" or "Read and write"), buckets, state ("Active", or "Missing from RustFS" for a key removed with another tool), creator, created.
- No row and no request but the create carries a secret.
- Single selection, admin only. Edit works on an active key; Delete works on any key.

**Create and edit** share one form: a name, the access level as a radio group (each option says what it allows and that buckets themselves are never touched), and a bucket `Multiselect` of the buckets that exist. A granted bucket that has since been deleted stays listed, so saving does not drop it silently. An edit says the secret stays the same.

**The secret** follows the private key rule of §6.5:
- `createAccessKey` goes through `uncachedRequest`, never an RTK mutation, so the answer never enters the Redux store. A test asserts the serialised store never contains it.
- `SecretKeyDialog` shows the access key and secret with copy buttons, plus `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_ENDPOINT_URL`/`AWS_REGION` lines. The endpoint and region come from the first granted bucket's detail.
- The dialog cannot be dismissed until "I have saved the secret access key" is ticked.
- It renders as a sibling of the form rather than inside it: a modal is portalled in the DOM but not in React, so its button would otherwise submit the form again.

### 6.9 Databases (managed Postgres)
Every Databases page renders inside `DatabasesGate` (`features/dbinstances`). It reads `/system/info`:
- With `databases.enabled: false`, the page is an info alert explaining `KUMO_DATABASES=on` instead of a table whose every request answers `503`.
- With the module on and `databases.engines` empty, the pages stay. That is the normal state of a fresh node: the list renders, **Create database** is disabled, and an info alert explains the one import an operator has to do (`packaging/images/postgres-18/`, then Import image with engine `postgres` and version `18`). Gating it would hide a working module behind a missing file.

**List.**
- Columns: Identifier (link), Status, Engine (`Postgres 18`), Endpoint (a dash until the guest has an address), Size, Storage, Created.
- Status is the **database's** state, not the server's (`07-databases.md` §6): `creating`/`starting`/`stopping`/`rebooting`/`deleting` are in-progress, `available` is success, `stopped` is stopped, `failed` is error. A guest that is running with an engine that never answered reads as **Failed**.
- Single selection, admin only. A `deleting` row cannot be selected. Header actions: Refresh, an **Actions** dropdown (Stop, Start, Reboot, Delete, each enabled only from the state it applies to) and **Create database**.
- It polls at 5 s while anything is transitional **or failed**, and 30 s otherwise. `failed` is polled because the reconciler publishes a database whose engine answered late — that is the one state an operator waits to see change on its own.

**Create.**
- Engine version (from `databases.engines`, newest first), identifier, admin user, password and confirmation, initial database, size and storage.
- Everything is checked locally first (`state.ts`), including the two rules a round trip would otherwise teach the hard way: a reserved Postgres name, and a password that is not printable ASCII. The ASCII rule is a correctness rule — SCRAM normalizes a password with SASLprep before deriving and Kumo does not, so another character would authenticate nowhere.
- **Next to the password field, and again as an alert: Kumo does not keep it.** There is no password reset and no way to show it again, so a database whose password is lost has to be created again (`07-databases.md` R8).
- A warning says connections are not encrypted and `sslmode=require` will not connect.
- The API answers `202` with the database `creating`, so the page opens its detail page, which explains what is happening.

**Detail.**
- **Details**: status, engine, size, compute, storage, created, and the **server's instance id** — the VM is on no Instances page and refuses every write, but the console log and the orphan report are keyed by that id.
- **Connect**: endpoint, port, database and admin user, plus a copyable `psql` command and a connection URI. Neither carries the password, and the panel says why: Kumo derived the engine's credential from it and discarded it, so `psql` prompts for it. The plain-connection warning repeats here, where someone is about to copy a command.
- **Activity**: the tasks filed under the DB instance id.
- A `failed` database gets an alert with the reason, the path of the guest's console log on the node, and what to do: try a reboot, else delete and create again. The server is left running on purpose.
- Stop, Start, Reboot and Delete are buttons, each enabled only from the state it applies to; a viewer sees them all disabled and the connection details enabled.
- A database that is gone (`404`) gets an info alert and a way back.

**Delete** is `DeleteDBInstanceModal`, shared by the list and the detail page. The data disk goes with the server and there is no snapshot, so it says so in as many words and the confirm button stays disabled until the identifier is typed exactly.

### 6.10 Kubernetes clusters
Every Kubernetes page renders inside `KubernetesGate` (`features/kubernetes`), the Databases gate's twin: `kubernetes.enabled: false` is an info alert explaining `KUMO_KUBERNETES=on`; the module on with `kubernetes.versions` empty keeps the pages, disables **Create cluster** and explains the import of the `kubernetes-1.37-<date>` release (or the recipe in `packaging/images/kubernetes-1.37/`).

**Status mapping** (`CLUSTER_INDICATOR`): `creating`, `updating`, `deleting` → in-progress; `active` → success; `degraded` → **warning**, not error -- the control plane answers and `state_reason` says what is wrong; `failed` → error.

**List.** Columns: Name (link), Status, Kubernetes (version), Nodes Ready (`ready / total` from the last health look), Endpoint (a dash until published), Created. Single selection, admin only; Delete and **Create cluster** in the header. It polls at 5 s while anything is transitional **or degraded** (the health pass brings a degraded cluster back on its own) and 30 s otherwise.

**Create.** Name, Kubernetes version (from `kubernetes.versions`), control plane size, worker size and worker count. The size pickers offer **only what kubeadm accepts** -- 2 vCPU for the control plane, 2048 MiB for every node -- so that refusal never comes from the server. An info alert says the cluster is reachable from inside its VPC only and that volumes use `local-path` on the nodes' disks. `202` lands on the detail page, which explains what is happening.

**Detail** has three tabs:
- **Overview**: status, version, Nodes Ready, control plane size, pod and service ranges, created, id; the node group table with an admin-only **Resize** (enabled from `active` or `degraded`); the cluster's tasks.
- **Nodes**: each VM with its role, Kubernetes readiness (`Ready`, `Not Ready`, or `Not yet seen` before the first look), VM state, address and instance id.
- **Connect**: **Download kubeconfig** (a POST: every click signs a new 30-day certificate, saved as `<name>.kubeconfig`; enabled once the cluster is up), the group it lands in (`kumo:admins` for an admin, `kumo:viewers` for a viewer -- a viewer downloads one too), the endpoint and a `kubectl` example, and an alert that pods do not get `NET_RAW` under CRI-O, so `ping` needs the capability added.
- `failed` gets an alert with the reason and where the nodes' console logs are; `degraded` one with the reason and that it recovers on its own; `creating` one saying the control plane boots first.

**Resize** is `ResizeNodeGroupModal`: 1-10 workers; shrinking adds an alert that the newest workers are drained first and that a PodDisruptionBudget holding a pod for 5 minutes stops the resize with the cluster degraded.

**Delete** is `DeleteClusterModal`: every node and everything on them goes, `local-path` volumes included, and the confirm button stays disabled until the name is typed exactly.

### 6.11 Spark clusters
Every Spark page renders inside `SparkGate` (`features/spark`), the Kubernetes gate's twin: `spark.enabled: false` is an info alert explaining `KUMO_SPARK=on`; the module on with `spark.versions` empty keeps the pages, disables **Create cluster** and explains the import of the `spark-4.3-<date>` release (or the recipe in `packaging/images/spark-4.3/`).

**Status mapping** (`SPARK_INDICATOR`): the Kubernetes one -- `creating`, `updating`, `deleting` → in-progress; `active` → success; `degraded` → **warning** (the master answers, `state_reason` says what is wrong); `failed` → error.

**List.** Columns: Name (link), Status, Spark (version), Workers alive (`alive / total`, from the master's last answer), Object storage (the buckets, or a dash), Master (the `spark://` URL, or a dash), Created. Polled at 5 s while anything is transitional or degraded.

**Create.** Name, Spark version (from `spark.versions`), master size (at least 2 vCPU and 4096 MiB), worker size (at least 2048 MiB), workers 1-10. **Object storage** is a checkbox shown only when `spark.object_storage` is true: checked, it reveals a bucket multiselect (from `GET /s3/buckets`) and an event-log select over the chosen buckets, defaulting to *None* (no event logs, no history server, 09-spark.md S10); unchecked or unavailable, the cluster runs without. The bucket rules are checked locally (`bucketsProblem`). A VPC-only alert explains that jobs are submitted from an instance in the VPC.

**Detail** has three tabs:
- **Overview**: status, version, workers alive, master size, the buckets, the event-log location (`s3a://<bucket>/spark-events/<id>`), created, id; the node group with an admin-only **Resize**; the cluster's tasks.
- **Nodes**: each VM with its role, what the master last said of it (`Alive`, `Not registered`, `Not yet seen`), VM state, address and instance id.
- **Connect**: for an admin, **Download client configuration** (a POST; the `spark-defaults.conf` with the cluster's secret and S3 key, saved as `kumo-<id>.conf`, never cached, enabled once the cluster is up); for a viewer, an alert that submitting needs the secret only an admin can download. The master URL and a `spark-submit` example to copy -- `--properties-file` and `--conf spark.driver.host=$(hostname -I | cut -d' ' -f1)` --, the master and history UI addresses, and an alert on why the driver host is passed and that the UIs have no sign-in.

**Resize** is `ResizeSparkNodeGroupModal`: 1-10 workers; shrinking warns that the newest workers are decommissioned first and that one not done within the drain timeout is powered off with the cluster degraded.

**Delete** is `DeleteSparkClusterModal`: every node and running job goes, with the cluster's access key; the buckets and event logs are named as kept; the name is typed to confirm.

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
- E2E (Playwright): `web/e2e/console.spec.ts` is login → import image → create key pair → launch → stop → terminate, and `web/e2e/databases.spec.ts` is import an engine image → create a database → read its Connect panel → stop → start → reboot → delete, both against the real binary on `KUMO_HYPERVISOR=fake` (`make e2e`). The databases journey also asserts the two guarantees the console cannot show on its own: that no response carries the admin password, and that the server behind a database is absent from the Instances pages and refuses their lifecycle endpoints with a `409`.
- The suite pins `KUMO_DEFAULT_VPC_CIDR` away from Kumo's default (`KUMO_E2E_VPC_CIDR`, `10.111.0.0/24`). The bootstrap refuses a default VPC that would shadow a host route, so on a machine whose own LAN is `192.168.100.0/24` — a common home-router range — the node comes up with no default subnet and every launch is refused. The specs read the same variable, so the address assertions follow it.
