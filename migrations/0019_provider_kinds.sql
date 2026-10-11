-- +goose Up
-- Providers now include every OpenAI-compatible service the editor offers
-- (Ollama, Groq, OpenRouter and others, providers.Catalog). The service
-- validates the kind; the table keeps only the identifier's shape.
alter table provider_connections drop constraint pc_provider_ck;
alter table provider_connections add constraint pc_provider_ck check (provider ~ '^[a-z][a-z0-9_]{1,40}$');
-- A custom connection to a local Ollama becomes an Ollama connection.
update provider_connections set provider = 'ollama'
 where provider = 'openai_compatible' and base_url ~ '^https?://(127\.0\.0\.1|localhost):11434(/|$)';

-- +goose Down
update provider_connections set provider = 'openai_compatible'
 where provider not in ('anthropic', 'openai', 'openai_compatible', 'gemini');
alter table provider_connections drop constraint pc_provider_ck;
alter table provider_connections add constraint pc_provider_ck check (provider in ('anthropic', 'openai', 'openai_compatible', 'gemini'));
