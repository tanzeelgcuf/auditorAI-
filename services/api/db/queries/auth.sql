-- name: CreateFirm :one
INSERT INTO firms (name) VALUES ($1) RETURNING *;

-- name: CreateUser :one
INSERT INTO users (firm_id, email, password_hash, role, email_verification_token, email_verification_expires)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetAssignedBooks :many
SELECT cb.* FROM client_books cb
JOIN user_book_assignments uba ON cb.id = uba.client_book_id
WHERE uba.user_id = $1;

-- name: VerifyEmail :one
UPDATE users SET email_verified = true,
    email_verification_token = NULL,
    email_verification_expires = NULL
WHERE id = $1 AND email_verification_token = $2
RETURNING *;

-- name: CreateBook :one
INSERT INTO client_books (firm_id, client_name) VALUES ($1, $2) RETURNING *;

-- name: ListBooksByFirm :many
SELECT * FROM client_books WHERE firm_id = $1 ORDER BY created_at DESC;
