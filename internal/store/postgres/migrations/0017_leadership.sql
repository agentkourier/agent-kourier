-- One row that says which replica leads (spec D37). Lead bumps the epoch when a replica gains the Kubernetes
-- Lease, and every write transaction of the store checks its own epoch against it first (inTx). The unique
-- index on a constant keeps it one row. Additive: a build without this table never reads or writes it.
CREATE TABLE leadership (
    epoch  bigint      NOT NULL,
    holder text        NOT NULL,
    since  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX leadership_one_row ON leadership ((true));
INSERT INTO leadership (epoch, holder) VALUES (0, '');
