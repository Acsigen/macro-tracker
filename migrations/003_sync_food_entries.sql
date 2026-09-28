UPDATE food_entries
SET (food_name, carbohydrate, total_sugar, free_sugar, protein, fat, fiber, salt) =
    (SELECT name, carbohydrate, total_sugar, free_sugar, protein, fat, fiber, salt
     FROM foods WHERE foods.id = food_entries.food_id)
WHERE food_id IS NOT NULL;

CREATE TRIGGER IF NOT EXISTS foods_sync_entries
AFTER UPDATE OF name, carbohydrate, total_sugar, free_sugar, protein, fat, fiber, salt ON foods
BEGIN
    UPDATE food_entries
    SET (food_name, carbohydrate, total_sugar, free_sugar, protein, fat, fiber, salt) =
        (NEW.name, NEW.carbohydrate, NEW.total_sugar, NEW.free_sugar, NEW.protein, NEW.fat, NEW.fiber, NEW.salt)
    WHERE food_id = NEW.id;
END;
