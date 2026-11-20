CREATE DATABASE IF NOT EXISTS hackathon;

CREATE TABLE prompts (
    id VARCHAR(64) NOT NULL,
    timestamp VARCHAR(64),
    prompt TEXT NOT NULL,
    tokens_in INT,
    tokens_out INT,
);

CREATE TABLE queue (
    prompt_id VARCHAR(64) NOT NULL,
    has_token_in BOOL,
    has_token_out BOOL,
    is_finished BOOL
)
