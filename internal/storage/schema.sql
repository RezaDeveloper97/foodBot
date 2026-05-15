-- recipe-bot MySQL schema.
-- Charset is utf8mb4 across the board so every character — Persian, emoji,
-- combining marks, the lot — round-trips correctly.

CREATE TABLE IF NOT EXISTS recipes (
    id                 INT UNSIGNED       NOT NULL,
    original_title     VARCHAR(500)       NOT NULL,
    title              VARCHAR(500)       NOT NULL,
    intro              TEXT,
    tip                TEXT,
    ready_in_minutes   INT UNSIGNED       NOT NULL DEFAULT 0,
    servings           INT UNSIGNED       NOT NULL DEFAULT 0,
    image_url          VARCHAR(1000),
    image_path         VARCHAR(500),
    formatted_content  MEDIUMTEXT         NOT NULL,
    status             ENUM('ready','published','failed') NOT NULL DEFAULT 'ready',
    error_message      TEXT,
    fetched_at         DATETIME           NOT NULL DEFAULT CURRENT_TIMESTAMP,
    published_at       DATETIME           NULL,
    PRIMARY KEY (id),
    KEY idx_status_fetched (status, fetched_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS recipe_ingredients (
    id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    recipe_id   INT UNSIGNED    NOT NULL,
    position    INT UNSIGNED    NOT NULL,
    original    VARCHAR(1000)   NOT NULL,
    localized   VARCHAR(1000)   NOT NULL,
    CONSTRAINT fk_ingredients_recipe FOREIGN KEY (recipe_id)
        REFERENCES recipes(id) ON DELETE CASCADE,
    KEY idx_recipe (recipe_id, position)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS recipe_steps (
    id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    recipe_id   INT UNSIGNED    NOT NULL,
    position    INT UNSIGNED    NOT NULL,
    original    TEXT            NOT NULL,
    localized   TEXT            NOT NULL,
    CONSTRAINT fk_steps_recipe FOREIGN KEY (recipe_id)
        REFERENCES recipes(id) ON DELETE CASCADE,
    KEY idx_recipe (recipe_id, position)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS publish_log (
    id            BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    recipe_id     INT UNSIGNED    NOT NULL,
    attempted_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    success       TINYINT(1)      NOT NULL,
    error_message TEXT,
    CONSTRAINT fk_log_recipe FOREIGN KEY (recipe_id)
        REFERENCES recipes(id) ON DELETE CASCADE,
    KEY idx_recipe (recipe_id, attempted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
