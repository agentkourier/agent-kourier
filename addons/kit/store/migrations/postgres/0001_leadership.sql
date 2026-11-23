-- One row that says which replica leads, and until when by the database's clock. A replica takes it once expires_at
-- has passed, or when holder already names it (a container that restarted in its pod); each take adds one to epoch,
-- and every write transaction, the add-on's and a library's that it fences (ADK's), checks its own epoch against the
-- row first, so a replica that lost the lease writes nothing. address is the leader's ADVERTISE_URL, where the standbys
-- forward A2A requests. The primary key on a constant keeps it one row, and no update changes it, so an update never
-- conflicts with the epoch check's KEY SHARE.
--
-- A database an add-on created before the kit kept this table, in the add-on's migration 0002, already has it and its
-- row, which a lease may hold: this migration then changes neither, and the kit's version table records it as applied.
CREATE TABLE IF NOT EXISTS leadership (
	id         boolean PRIMARY KEY DEFAULT true CHECK (id),
	holder     text NOT NULL,
	address    text NOT NULL,
	epoch      bigint NOT NULL,
	expires_at timestamptz NOT NULL,
	since      timestamptz NOT NULL
);
INSERT INTO leadership (holder, address, epoch, expires_at, since) VALUES ('', '', 0, '-infinity', now())
	ON CONFLICT (id) DO NOTHING;
