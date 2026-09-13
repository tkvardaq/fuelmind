-- 009_messages.down.sql
DROP INDEX IF EXISTS idx_messages_time;
DROP INDEX IF EXISTS idx_messages_queue;
DROP TABLE IF EXISTS messages;
