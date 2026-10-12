-- +goose Up
-- Private previews (docs/12-routing-and-https.md): only people who sign in
-- with GitHub and can see the repository may open its previews. The router
-- asks the server (forward authentication) before every request.
ALTER TABLE repos ADD COLUMN private_previews BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE repos DROP COLUMN private_previews;
