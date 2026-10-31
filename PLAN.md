# PLAN.md

## Phase 1: Implement ResourcePrefix and table-driven tests

Add the exported function `ResourcePrefix(value string) (string, bool)` to the existing file `internal/id/resource.go`, alongside the existing `HasResourceShape`, `Resource`, and `Prefix*` constants. The function iterates the seven `Prefix*` constants (`PrefixInstance`, `PrefixImage`, `PrefixKeyPair`, `PrefixTask`, `PrefixVPC`, `PrefixSubnet`, `PrefixNetworkInterface`) and calls `HasResourceShape(prefix, value)` for each; on the first match it returns `(prefix, true)`, and if no prefix matches it returns `("", false)`. It must reuse `HasResourceShape` and the existing constants — no new logic for shape validation.

Add table-driven tests to the existing `internal/id/resource_test.go` (in the same `id_test` package) covering: a valid ID for each of the seven prefixes (e.g. `"i-0a1b2c3d4e5f60718"`, `"img-0a1b2c3d4e5f60718"`, `"key-0a1b2c3d4e5f60718"`, `"task-0a1b2c3d4e5f60718"`, `"vpc-0a1b2c3d4e5f60718"`, `"subnet-0a1b2c3d4e5f60718"`, `"eni-0a1b2c3d4e5f60718"`), an unknown prefix (`"foo-0a1b2c3d4e5f60718"`), a value one hex character too short (`"i-0a1b2c3d4e5f6071"`), a value with an uppercase hex character (`"i-0A1B2C3D4E5F60718"`), and the empty string (`""`). Each test case should assert the expected `(prefix, ok)` tuple.

Files modified: `internal/id/resource.go` (add function), `internal/id/resource_test.go` (add test function). No new files, no new dependencies, no changes to existing functions or call sites.
