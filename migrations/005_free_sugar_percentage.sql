DROP TRIGGER IF EXISTS foods_sync_entries;

ALTER TABLE foods ADD COLUMN free_sugar_percent REAL NOT NULL DEFAULT 0 CHECK (free_sugar_percent BETWEEN 0 AND 100);
ALTER TABLE food_entries ADD COLUMN free_sugar_percent REAL NOT NULL DEFAULT 0 CHECK (free_sugar_percent BETWEEN 0 AND 100);

CREATE TRIGGER foods_sync_entries
AFTER UPDATE OF name, carbohydrate, total_sugar, free_sugar_percent, protein, fat, fiber, salt ON foods
BEGIN
    UPDATE food_entries
    SET (food_name, carbohydrate, total_sugar, free_sugar_percent, protein, fat, fiber, salt) =
        (NEW.name, NEW.carbohydrate, NEW.total_sugar, NEW.free_sugar_percent, NEW.protein, NEW.fat, NEW.fiber, NEW.salt)
    WHERE food_id = NEW.id;
END;
