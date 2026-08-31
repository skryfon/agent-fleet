-- name: GetIdentityByZulipUserID :one
SELECT * FROM identity WHERE zulip_user_id = $1;

-- name: GetIdentityByGithubLogin :one
SELECT * FROM identity WHERE github_login = $1;

-- name: CreateIdentity :one
INSERT INTO identity (kind, display_name, zulip_user_id, github_login, role)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;
