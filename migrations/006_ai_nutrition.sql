DROP TRIGGER IF EXISTS foods_sync_entries;

CREATE TABLE ai_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    base_url TEXT NOT NULL DEFAULT 'https://deepseek.dait.es/v1',
    model TEXT NOT NULL DEFAULT '',
    api_key BLOB
);
INSERT INTO ai_settings(id) VALUES(1);

ALTER TABLE foods ADD COLUMN omega3 REAL CHECK (omega3 IS NULL OR omega3 BETWEEN 0 AND 100);
ALTER TABLE foods ADD COLUMN omega6 REAL CHECK (omega6 IS NULL OR omega6 BETWEEN 0 AND 100);
ALTER TABLE foods ADD COLUMN analysis_source TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE foods ADD COLUMN analysis_model TEXT NOT NULL DEFAULT '';
ALTER TABLE foods ADD COLUMN analyzed_at TEXT NOT NULL DEFAULT '';
ALTER TABLE foods ADD COLUMN assumptions TEXT NOT NULL DEFAULT '';

ALTER TABLE food_entries ADD COLUMN omega3 REAL CHECK (omega3 IS NULL OR omega3 BETWEEN 0 AND 100);
ALTER TABLE food_entries ADD COLUMN omega6 REAL CHECK (omega6 IS NULL OR omega6 BETWEEN 0 AND 100);
ALTER TABLE food_entries ADD COLUMN analysis_source TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE food_entries ADD COLUMN analysis_model TEXT NOT NULL DEFAULT '';
ALTER TABLE food_entries ADD COLUMN analyzed_at TEXT NOT NULL DEFAULT '';
ALTER TABLE food_entries ADD COLUMN assumptions TEXT NOT NULL DEFAULT '';
