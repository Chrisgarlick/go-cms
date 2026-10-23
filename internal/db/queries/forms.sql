-- name: InsertFormSubmission :exec
INSERT INTO form_submissions (id, form, data_enc, summary) VALUES ($1, $2, $3, $4);

-- name: GetFormSubmission :one
SELECT * FROM form_submissions WHERE id = $1;

-- name: ListFormSubmissions :many
SELECT * FROM form_submissions WHERE form = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3;

-- name: FormSubmissionCounts :many
SELECT form, count(*)::bigint AS total, count(*) FILTER (WHERE read_at IS NULL)::bigint AS unread
FROM form_submissions GROUP BY form;

-- name: MarkFormSubmissionRead :exec
UPDATE form_submissions SET read_at = now() WHERE id = $1 AND read_at IS NULL;

-- name: DeleteFormSubmission :execrows
DELETE FROM form_submissions WHERE id = $1;

-- name: PruneFormSubmissions :execrows
DELETE FROM form_submissions WHERE form = @form::text AND created_at < now() - (@age::text)::interval;
