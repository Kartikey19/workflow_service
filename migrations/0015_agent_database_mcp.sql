-- +goose Up
alter table agents add column database_connection text not null default '';
alter table connections drop constraint connections_kind_ck;
alter table connections add constraint connections_kind_ck check (kind in
  ('smtp', 'resend', 'gmail', 'gdrive', 'dropbox', 'onedrive', 'postgres', 'mysql', 'mcp'));

-- +goose Down
alter table connections drop constraint connections_kind_ck;
alter table connections add constraint connections_kind_ck check (kind in
  ('smtp', 'resend', 'gmail', 'gdrive', 'dropbox', 'onedrive', 'postgres', 'mysql'));
alter table agents drop column database_connection;
