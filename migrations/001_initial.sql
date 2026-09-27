CREATE TABLE profile (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    name TEXT NOT NULL DEFAULT '',
    height_cm REAL CHECK (height_cm IS NULL OR height_cm > 0),
    energy_target REAL NOT NULL DEFAULT 2000 CHECK (energy_target > 0)
);

INSERT INTO profile (id) VALUES (1);

CREATE TABLE foods (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL COLLATE NOCASE UNIQUE,
    carbohydrate REAL NOT NULL CHECK (carbohydrate BETWEEN 0 AND 100),
    total_sugar REAL NOT NULL CHECK (total_sugar BETWEEN 0 AND carbohydrate),
    free_sugar REAL CHECK (free_sugar IS NULL OR free_sugar BETWEEN 0 AND total_sugar),
    protein REAL NOT NULL CHECK (protein BETWEEN 0 AND 100),
    fiber REAL NOT NULL CHECK (fiber BETWEEN 0 AND 100),
    salt REAL NOT NULL CHECK (salt BETWEEN 0 AND 100),
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE food_entries (
    id INTEGER PRIMARY KEY,
    food_id INTEGER REFERENCES foods(id) ON DELETE SET NULL,
    entry_date TEXT NOT NULL,
    consumed_g REAL NOT NULL CHECK (consumed_g > 0),
    food_name TEXT NOT NULL,
    carbohydrate REAL NOT NULL,
    total_sugar REAL NOT NULL,
    free_sugar REAL,
    protein REAL NOT NULL,
    fiber REAL NOT NULL,
    salt REAL NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX food_entries_date_idx ON food_entries(entry_date);

CREATE TABLE body_entries (
    id INTEGER PRIMARY KEY,
    entry_date TEXT NOT NULL UNIQUE,
    weight_kg REAL NOT NULL CHECK (weight_kg > 0),
    waist_cm REAL NOT NULL CHECK (waist_cm > 0),
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sleep_entries (
    id INTEGER PRIMARY KEY,
    entry_date TEXT NOT NULL UNIQUE,
    bed_time TEXT NOT NULL,
    wake_time TEXT NOT NULL,
    duration_hours REAL NOT NULL CHECK (duration_hours > 0 AND duration_hours <= 24),
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
