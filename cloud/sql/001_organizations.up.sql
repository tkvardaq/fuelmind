-- 001_organizations.up.sql
--
-- Cloud control plane (spec §6). Deployed to a managed Postgres
-- instance on Hetzner (see build plan decision O2). This migration
-- is the first of five; the others follow as 002..005.
--
-- Spec §6.1: organizations are the top-level multi-tenant boundary.
-- v1 ships with one synthetic "fuelmind-pilots" org that all
-- pilot stations belong to. v2/HQ adds a real admin UI for org
-- management.
--
-- Why UUIDs everywhere? Spec §6.1 uses UUID for orgs and licenses
-- (not for stations — those are short codes per spec). We mirror
-- the spec exactly; mixing in bigint would be a needless deviation.

CREATE TABLE organizations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name             TEXT NOT NULL,
    contact_email    TEXT,
    created_at       TIMESTAMP NOT NULL DEFAULT now()
);