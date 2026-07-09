-- Restore the fork's original NOT NULL DEFAULT '' shape for agent.model.
UPDATE agent SET model = '' WHERE model IS NULL;
ALTER TABLE agent ALTER COLUMN model SET DEFAULT '';
ALTER TABLE agent ALTER COLUMN model SET NOT NULL;
