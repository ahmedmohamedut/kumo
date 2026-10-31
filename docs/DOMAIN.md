# Domain Model — kumo

Resources: instances (`i-`), images (`img-`), key pairs (`key-`), tasks (`task-`) — each ID is the
prefix plus 17 hex characters from `internal/id`; users and internal rows use UUIDv7. Object
storage adds buckets and access keys. The authoritative description is
`docs/design/02-data-model.md`; extend this file as the context-agent learns more.
