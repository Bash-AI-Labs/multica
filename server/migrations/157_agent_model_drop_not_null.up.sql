-- Reconcile fork/upstream drift on agent.model. The fork's 033_agent_model
-- added the column as NOT NULL DEFAULT '', but upstream treats agent.model as a
-- nullable TEXT column (pgtype.Text in the generated code). Relaxing the
-- constraint makes the merged schema match upstream and converges existing fork
-- databases (which already applied 033) with freshly-migrated ones.
ALTER TABLE agent ALTER COLUMN model DROP DEFAULT;
ALTER TABLE agent ALTER COLUMN model DROP NOT NULL;
