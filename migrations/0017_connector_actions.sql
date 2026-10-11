-- +goose Up
alter table connections drop constraint connections_kind_ck;
alter table connections add constraint connections_kind_ck check (kind in
 ('smtp','resend','gmail','gdrive','gdocs','gsheets','gcalendar','dropbox','onedrive','postgres','mysql','mongo','slack','notion','mcp'));

-- +goose Down
alter table connections drop constraint connections_kind_ck;
alter table connections add constraint connections_kind_ck check (kind in
 ('smtp','resend','gmail','gdrive','dropbox','onedrive','postgres','mysql','mcp'));
