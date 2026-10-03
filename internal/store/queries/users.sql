-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6)
RETURNING id, username, password_hash, role, created_at, updated_at;

-- name: GetUserByUsername :one
SELECT id, username, password_hash, role, created_at, updated_at
FROM users
WHERE username = ?1;

-- name: GetUserByID :one
SELECT id, username, password_hash, role, created_at, updated_at
FROM users
WHERE id = ?1;

-- name: UpdateUserPassword :one
UPDATE users
SET password_hash = ?2, updated_at = ?3
WHERE id = ?1
RETURNING id, username, password_hash, role, created_at, updated_at;

-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (id, user_id, token_hash, family_id, expires_at, user_agent, created_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
RETURNING id, user_id, token_hash, family_id, expires_at, user_agent, revoked_at, created_at;

-- name: GetRefreshTokenByHash :one
SELECT id, user_id, token_hash, family_id, expires_at, user_agent, revoked_at, created_at
FROM refresh_tokens
WHERE token_hash = ?1;

-- name: RevokeRefreshToken :one
UPDATE refresh_tokens
SET revoked_at = ?2
WHERE id = ?1 AND revoked_at IS NULL
RETURNING id, user_id, token_hash, family_id, expires_at, user_agent, revoked_at, created_at;

-- name: RevokeRefreshTokenFamily :execrows
UPDATE refresh_tokens
SET revoked_at = ?2
WHERE family_id = ?1 AND revoked_at IS NULL;

-- name: RevokeRefreshTokensForUser :execrows
UPDATE refresh_tokens
SET revoked_at = ?2
WHERE user_id = ?1 AND revoked_at IS NULL;

-- name: DeleteExpiredRefreshTokens :execrows
DELETE FROM refresh_tokens
WHERE expires_at < ?1;