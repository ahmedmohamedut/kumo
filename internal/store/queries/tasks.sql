-- name: CreateTask :one
INSERT INTO tasks (
  id, type, resource_id, state, progress, payload, error, attempts, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10
)
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = ?1;

-- name: ListTasksByResource :many
SELECT * FROM tasks WHERE resource_id = ?1 ORDER BY created_at DESC;

-- name: UpdateTask :one
UPDATE tasks
SET state = ?2,
    progress = ?3,
    error = ?4,
    attempts = ?5,
    updated_at = ?6
WHERE id = ?1
RETURNING *;

-- Progress is reported by the running handler, so it must not rewrite state,
-- error or attempts. A workflow that cannot measure progress repeats the last
-- value it reported.
-- name: UpdateTaskProgress :one
UPDATE tasks
SET progress = ?2,
    updated_at = ?3
WHERE id = ?1
RETURNING *;

-- Claiming is a single UPDATE guarded by the row's own state, so two workers
-- racing for the same task cannot both win: the loser matches no row and gets
-- sql.ErrNoRows. Ordering by created_at keeps the queue FIFO.
-- name: ClaimPendingTask :one
UPDATE tasks
SET state = 'running',
    attempts = attempts + 1,
    updated_at = sqlc.arg(updated_at)
WHERE id = (
  SELECT id FROM tasks
  WHERE state = 'pending'
  ORDER BY created_at ASC, id ASC
  LIMIT 1
)
RETURNING *;

-- Restart recovery: tasks left 'running' by a crash or a kill belong to no
-- worker anymore, so the pool hands them back to the queue. progress is reset
-- because the previous attempt's partial work counts for nothing.
-- name: RequeueRunningTasks :many
UPDATE tasks
SET state = 'pending',
    progress = 0,
    error = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE state = 'running'
RETURNING *;

-- name: ListPendingTasks :many
SELECT * FROM tasks WHERE state = 'pending' ORDER BY created_at ASC;

-- Keyset pagination over (created_at, id), the same shape as the image listing.
-- name: ListTasksDesc :many
SELECT * FROM tasks
WHERE (sqlc.arg(cursor_created) = ''
       OR created_at < sqlc.arg(cursor_created)
       OR (created_at = sqlc.arg(cursor_created) AND id < sqlc.arg(cursor_id)))
  AND (sqlc.narg(resource_id) IS NULL OR resource_id = sqlc.narg(resource_id))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListTasksAsc :many
SELECT * FROM tasks
WHERE (sqlc.arg(cursor_created) = ''
       OR created_at > sqlc.arg(cursor_created)
       OR (created_at = sqlc.arg(cursor_created) AND id > sqlc.arg(cursor_id)))
  AND (sqlc.narg(resource_id) IS NULL OR resource_id = sqlc.narg(resource_id))
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(page_limit);
