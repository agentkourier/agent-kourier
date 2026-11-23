-- This migration made the A2A tasks, the message IDs of the user messages that started or joined them, and when each
-- context was last active. The kit's migration 0002 has them now, and runs before the agent's: on a database this
-- migration already built, the kit's adopts the tables as they are and adds the context's ref. It stays, as a
-- statement that does nothing, so that the version list does not change.
SELECT 1;
