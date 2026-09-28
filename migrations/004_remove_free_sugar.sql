DROP TRIGGER IF EXISTS foods_sync_entries;

ALTER TABLE foods DROP COLUMN free_sugar;
ALTER TABLE food_entries DROP COLUMN free_sugar;

CREATE TRIGGER foods_sync_entries
AFTER UPDATE OF name, carbohydrate, total_sugar, protein, fat, fiber, salt ON foods
BEGIN
    UPDATE food_entries
    SET (food_name, carbohydrate, total_sugar, protein, fat, fiber, salt) =
        (NEW.name, NEW.carbohydrate, NEW.total_sugar, NEW.protein, NEW.fat, NEW.fiber, NEW.salt)
    WHERE food_id = NEW.id;
END;
