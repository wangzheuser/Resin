ALTER TABLE subscriptions ADD COLUMN download_mode TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE subscriptions ADD COLUMN download_platform_id TEXT NOT NULL DEFAULT '';
